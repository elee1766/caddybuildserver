package xcaddy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/sandbox"
)

// commandOptions represents options for running a command in the sandbox
type commandOptions struct {
	command      string
	args         []string
	allowNetwork bool
}

// environment represents a build environment for compiling Caddy
type environment struct {
	caddyVersion    string
	plugins         []Plugin
	caddyModulePath string
	tempFolder      string
	timeoutGoGet    time.Duration
	skipCleanup     bool
	logger          *slog.Logger
	stdout          io.Writer
	stderr          io.Writer
	env             map[string]string      // Environment variables to use for all commands
	sandbox         *sandbox.DockerSandbox // Docker sandbox for isolated builds
	outputBuffer    *bytes.Buffer          // Captures output for error analysis
}

// Close cleans up the build environment
// Note: We don't delete tempFolder since it's owned by the caller (builder.go creates and manages it)
func (env *environment) Close() error {
	// Close the Docker client
	if env.sandbox != nil {
		return env.sandbox.Close()
	}
	return nil
}

// runCommand executes a command in the Docker sandbox
func (env *environment) runCommand(ctx context.Context, opts commandOptions) error {
	var timeout time.Duration
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}

	// Build full command string for logging
	fullCmd := opts.command
	if len(opts.args) > 0 {
		fullCmd = opts.command + " " + strings.Join(opts.args, " ")
	}

	env.logger.Info("executing sandboxed command",
		"command", fullCmd,
		"args", opts.args,
		"allow_network", opts.allowNetwork,
		"timeout", timeout,
	)

	// Use custom writers if provided, otherwise use os.Stdout/Stderr
	stdout := env.stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := env.stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Also write to buffer for error analysis
	stdoutMulti := io.MultiWriter(stdout, env.outputBuffer)
	stderrMulti := io.MultiWriter(stderr, env.outputBuffer)

	// Set environment variables in sandbox
	for key, value := range env.env {
		env.sandbox.AddEnv(key, value)
	}

	// Run command in Docker sandbox with specified network access
	err := env.sandbox.RunCommandWithNetwork(ctx, opts.command, opts.args, env.tempFolder, stdoutMulti, stderrMulti, opts.allowNetwork)
	if err != nil {
		// Classify the error based on output
		output := env.outputBuffer.String()
		isFatal := classifyBuildError(output)
		return &BuildError{
			Err:    err,
			Output: output,
			Fatal:  isFatal,
		}
	}
	return nil
}

// runGoCommand executes a go command in the Docker sandbox without network access
func (env *environment) runGoCommand(ctx context.Context, subcommand string, args ...string) error {
	allArgs := append([]string{subcommand}, args...)
	return env.runCommand(ctx, commandOptions{
		command:      "go",
		args:         allArgs,
		allowNetwork: false,
	})
}

// runGoCommandWithNetwork executes a go command in the Docker sandbox with optional network access
func (env *environment) runGoCommandWithNetwork(ctx context.Context, allowNetwork bool, subcommand string, args ...string) error {
	allArgs := append([]string{subcommand}, args...)
	return env.runCommand(ctx, commandOptions{
		command:      "go",
		args:         allArgs,
		allowNetwork: allowNetwork,
	})
}

// execGoGet runs "go get" with the given module/version
// Network is required for downloading dependencies
func (env *environment) execGoGet(ctx context.Context, modulePath, moduleVersion, caddyModulePath, caddyVersion string) error {
	mod := modulePath
	if moduleVersion != "" {
		mod += "@" + moduleVersion
	}
	caddy := caddyModulePath
	if caddyVersion != "" {
		caddy += "@" + caddyVersion
	}

	args := []string{"get", "-d"}
	if caddy != "" {
		args = append(args, mod, caddy)
	} else {
		args = append(args, mod)
	}

	return env.runCommand(ctx, commandOptions{
		command:      "go",
		args:         args,
		allowNetwork: true, // Enable network for go get
	})
}

// newEnvironment creates and initializes a new build environment
func newEnvironment(ctx context.Context, b *Builder, buildDir string, logger *slog.Logger) (*environment, error) {
	// Determine Caddy module path (v2 or v3)
	caddyModulePath := "github.com/caddyserver/caddy"
	if !strings.HasPrefix(b.CaddyVersion, "v") || !strings.Contains(b.CaddyVersion, ".") {
		caddyModulePath += "/v2"
	}

	// Apply semantic import versioning
	var err error
	caddyModulePath, err = versionedModulePath(caddyModulePath, b.CaddyVersion)
	if err != nil {
		return nil, err
	}

	// Clean up plugin paths for semantic import versioning
	for i, p := range b.Plugins {
		b.Plugins[i].PackagePath, err = versionedModulePath(p.PackagePath, p.Version)
		if err != nil {
			return nil, err
		}
	}

	// Create template context
	tplCtx := mainModuleContext{
		CaddyModule: caddyModulePath,
	}
	for _, p := range b.Plugins {
		tplCtx.Plugins = append(tplCtx.Plugins, p.PackagePath)
	}

	// Render main module template
	var buf bytes.Buffer
	tpl, err := template.New("main").Parse(mainModuleTemplate)
	if err != nil {
		return nil, err
	}
	err = tpl.Execute(&buf, tplCtx)
	if err != nil {
		return nil, err
	}

	// Use the provided build directory
	logger.Info("using build directory", "path", buildDir)

	// Write main.go
	mainPath := filepath.Join(buildDir, "main.go")
	logger.Info("writing main module", "path", mainPath)
	logger.Debug("main module content", "content", buf.String())
	err = os.WriteFile(mainPath, buf.Bytes(), 0644)
	if err != nil {
		return nil, err
	}

	// Create Docker sandbox for isolated builds
	dockerConfig := sandbox.DefaultDockerConfig()
	dockerConfig.DockerHost = b.DockerHost

	// Override image if specified
	if b.DockerImage != "" {
		dockerConfig.Image = b.DockerImage
	}

	// Override resource limits if specified
	if b.DockerMemoryMB > 0 {
		dockerConfig.MemoryLimit = b.DockerMemoryMB * 1024 * 1024 // Convert MB to bytes
	}
	if b.DockerCPUs > 0 {
		// Convert cores to CPU quota (microseconds per 100ms period)
		// 1 core = 100000 microseconds, 2 cores = 200000, etc.
		dockerConfig.CPUQuota = int64(b.DockerCPUs * 100000)
	}
	if b.DockerPIDsLimit > 0 {
		dockerConfig.PidsLimit = b.DockerPIDsLimit
	}

	if b.DockerHost != "" {
		logger.Info("using custom Docker host", "docker_host", b.DockerHost)
	} else {
		logger.Info("using default Docker host from environment or default socket")
	}

	logger.Info("using Docker configuration",
		"image", dockerConfig.Image,
		"memory_mb", dockerConfig.MemoryLimit/(1024*1024),
		"cpus", float64(dockerConfig.CPUQuota)/100000,
		"pids_limit", dockerConfig.PidsLimit,
	)

	dockerSandbox, err := sandbox.NewDockerSandbox(dockerConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker sandbox: %w", err)
	}

	// Pull the Docker image
	logger.Info("pulling Docker image", "image", dockerConfig.Image)
	if err := dockerSandbox.PullImage(ctx); err != nil {
		return nil, fmt.Errorf("failed to pull Docker image: %w", err)
	}

	env := &environment{
		caddyVersion:    b.CaddyVersion,
		plugins:         b.Plugins,
		caddyModulePath: caddyModulePath,
		tempFolder:      buildDir,
		timeoutGoGet:    b.TimeoutGet,
		skipCleanup:     b.SkipCleanup,
		logger:          logger,
		stdout:          b.Stdout,
		stderr:          b.Stderr,
		env:             b.buildEnv(), // Use the minimal environment from builder
		sandbox:         dockerSandbox,
		outputBuffer:    &bytes.Buffer{}, // Buffer to capture output for error analysis
	}

	// Initialize go module
	logger.Info("initializing go module")
	err = env.runGoCommand(ctx, "mod", "init", "caddy")
	if err != nil {
		return nil, err
	}

	// Apply module replacements before pinning versions
	if len(b.Replacements) > 0 {
		logger.Info("applying module replacements")
		args := []string{"mod", "edit"}
		for _, r := range b.Replacements {
			logger.Info("replacing module", "old", r.Old, "new", r.New)
			args = append(args, "-replace", r.String())
		}
		err := env.runCommand(ctx, commandOptions{
			command:      "go",
			args:         args,
			allowNetwork: false, // No network needed for go mod edit
		})
		if err != nil {
			return nil, err
		}
	}

	// Create separate context with timeout for go get operations
	if env.timeoutGoGet > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), env.timeoutGoGet)
		defer cancel()
	}

	// Pin versions - first Caddy itself
	logger.Info("pinning versions")
	err = env.execGoGet(ctx, caddyModulePath, env.caddyVersion, "", "")
	if err != nil {
		return nil, err
	}

	// Then each plugin
	for _, p := range b.Plugins {
		err = env.execGoGet(ctx, p.PackagePath, p.Version, caddyModulePath, env.caddyVersion)
		if err != nil {
			return nil, err
		}

		// Check for context cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}

	// Final empty go get to resolve any ambiguities
	err = env.execGoGet(ctx, "", "", "", "")
	if err != nil {
		return nil, err
	}

	logger.Info("build environment ready")
	return env, nil
}

// mainModuleContext is the template context for the main module
type mainModuleContext struct {
	CaddyModule string
	Plugins     []string
}

// mainModuleTemplate is the Go template for the main.go file
const mainModuleTemplate = `package main

import (
	caddycmd "{{.CaddyModule}}/cmd"

	// plug in Caddy modules here
	_ "{{.CaddyModule}}/modules/standard"
	{{- range .Plugins}}
	_ "{{.}}"
	{{- end}}
)

func main() {
	caddycmd.Main()
}
`
