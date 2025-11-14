package xcaddy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Build builds Caddy with the configured version and plugins, outputting the binary to outputFile
func (b *Builder) Build(ctx context.Context, outputFile string, logger *slog.Logger) error {
	// Apply build timeout if configured
	var cancel context.CancelFunc
	if b.TimeoutBuild > 0 {
		ctx, cancel = context.WithTimeout(ctx, b.TimeoutBuild)
		defer cancel()
	}

	if outputFile == "" {
		return fmt.Errorf("output file path is required")
	}

	// Convert output file to absolute path
	absOutputFile, err := filepath.Abs(outputFile)
	if err != nil {
		return err
	}

	// Extract the directory from the output file path
	// This will be used as the build directory
	buildDir := filepath.Dir(absOutputFile)

	logger.Info("building caddy", "output", absOutputFile, "build_dir", buildDir, "version", b.CaddyVersion)

	// Set OS/Arch defaults from environment if not specified
	if b.OS == "" {
		b.OS = getEnvOrDefault("GOOS", runtime.GOOS)
	}
	if b.Arch == "" {
		b.Arch = getEnvOrDefault("GOARCH", runtime.GOARCH)
	}
	if b.ARM == "" {
		b.ARM = os.Getenv("GOARM")
	}

	// Prepare build environment using the directory from outputFile
	buildEnv, err := newEnvironment(ctx, b, buildDir, logger)
	if err != nil {
		return err
	}
	defer buildEnv.Close()

	// Race detector requires CGO
	if b.RaceDetector && !b.Cgo {
		logger.Warn("enabling cgo because it is required by the race detector")
		b.Cgo = true
	}

	// Run go mod tidy to ensure go.mod and go.sum are consistent
	// Network is required to download any missing dependencies
	logger.Info("tidying go modules")
	if err := buildEnv.runGoCommandWithNetwork(ctx, true, "mod", "tidy", "-e"); err != nil {
		return err
	}

	// Build the binary
	// Use just the filename since Docker will run this in /workspace which is mounted to buildDir
	outputFileName := filepath.Base(absOutputFile)
	logger.Info("compiling caddy binary", "output_file", outputFileName)
	args := []string{"build", "-o", outputFileName}

	// Add build flags
	if b.Debug {
		// Support for debuggers like dlv
		args = append(args, "-gcflags", "all=-N -l")
	} else {
		// Trim debug symbols and build info
		args = append(args,
			"-ldflags", "-w -s",
			"-trimpath",
		)
	}

	// Add race detector flag if requested
	if b.RaceDetector {
		args = append(args, "-race")
	}

	err = buildEnv.runCommand(ctx, commandOptions{
		command:      "go",
		args:         args,
		allowNetwork: false, // No network needed for build
	})
	if err != nil {
		return err
	}

	logger.Info("build complete", "output", absOutputFile)
	return nil
}

// buildEnv creates a minimal environment map for the build process
// These are passed to the Docker sandbox (NOT from host environment)
func (b *Builder) buildEnv() map[string]string {
	env := make(map[string]string)

	// Build-specific environment variables
	env["GOOS"] = b.OS
	env["GOARCH"] = b.Arch
	if b.ARM != "" {
		env["GOARM"] = b.ARM
	}

	// Set CGO_ENABLED
	cgoEnabled := "0"
	if b.Cgo || b.RaceDetector {
		cgoEnabled = "1"
	}
	env["CGO_ENABLED"] = cgoEnabled

	// Add any additional environment variables from configuration
	for key, value := range b.AdditionalEnv {
		// Skip if trying to override critical vars
		if !isCriticalEnvVar(key) {
			env[key] = value
		}
	}

	return env
}

// isCriticalEnvVar returns true for environment variables that should not be overridden
func isCriticalEnvVar(key string) bool {
	critical := []string{"GOOS", "GOARCH", "GOARM", "CGO_ENABLED"}
	for _, c := range critical {
		if strings.EqualFold(key, c) {
			return true
		}
	}
	return false
}

// getEnvOrDefault returns the environment variable value or a default
func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
