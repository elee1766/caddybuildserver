package builder

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/db"
	"github.com/elee1766/caddybuildserver/pkg/storage"
	"github.com/elee1766/caddybuildserver/pkg/xcaddy"
)

// BuildRequest represents a request to build Caddy with specific plugins
type BuildRequest struct {
	BuildKey     string   // Unique key for this build
	CaddyVersion string   // e.g., "v2.7.6" or "latest"
	Plugins      []string // e.g., ["github.com/caddy-dns/cloudflare@v0.0.1"]
	Replacements []string // e.g., ["github.com/caddyserver/caddy/v2=../caddy"]
	OS           string   // e.g., "linux"
	Arch         string   // e.g., "amd64"
	Attempt      int      // Current attempt number for this build
	StorageKey   string   // Key where the binary should be stored
}

// BuildResult contains the results of a build operation
type BuildResult struct {
	StorageKey   string // Key where the binary was stored
	HasSignature bool   // Whether a signature was generated and stored
}

// Builder handles building Caddy binaries with xcaddy
type Builder struct {
	workDir         string
	storage         *storage.Storage
	db              db.ExecBatchQuerier
	logger          *slog.Logger
	additionalEnv   map[string]string // Additional env vars to pass to build process
	dockerHost      string            // Docker daemon socket path
	dockerImage     string            // Docker image to use for builds
	dockerMemoryMB  int64             // Memory limit in MB
	dockerCPUs      float64           // CPU limit in cores
	dockerPIDsLimit int64             // PID limit
	signer          *Signer           // Optional PGP signer for signing binaries
}

// New creates a new Builder instance
func New(workDir string, stor *storage.Storage, database db.ExecBatchQuerier, additionalEnv map[string]string, dockerHost string, dockerImage string, dockerMemoryMB int64, dockerCPUs float64, dockerPIDsLimit int64, signingEnabled bool, signingKeyFile string, signingKeyPassword string, logger *slog.Logger) (*Builder, error) {
	// Create work directory if it doesn't exist
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Initialize signer if signing is enabled
	var signer *Signer
	if signingEnabled {
		if signingKeyFile == "" {
			return nil, fmt.Errorf("signing is enabled but no key_file configured")
		}
		logger.Info("loading signing key", "key_file", signingKeyFile)
		var err error
		signer, err = NewSigner(signingKeyFile, signingKeyPassword)
		if err != nil {
			// Fail to start if signing is enabled but key cannot be loaded
			return nil, fmt.Errorf("failed to load signing key (signing is enabled): %w", err)
		}
		logger.Info("binary signing enabled", "key_file", signingKeyFile)
	} else {
		logger.Info("binary signing disabled")
	}

	return &Builder{
		workDir:         workDir,
		storage:         stor,
		db:              database,
		logger:          logger,
		additionalEnv:   additionalEnv,
		dockerHost:      dockerHost,
		dockerImage:     dockerImage,
		dockerMemoryMB:  dockerMemoryMB,
		dockerCPUs:      dockerCPUs,
		dockerPIDsLimit: dockerPIDsLimit,
		signer:          signer,
	}, nil
}

// Build builds Caddy with the specified configuration and returns the storage keys
func (b *Builder) Build(ctx context.Context, req BuildRequest) (*BuildResult, error) {
	// Log build start
	b.logger.Info("starting build",
		"build_key", req.BuildKey,
		"attempt", req.Attempt,
		"version", req.CaddyVersion,
		"os", req.OS,
		"arch", req.Arch,
		"plugins", req.Plugins,
		"replacements", req.Replacements,
		"storage_key", req.StorageKey,
	)

	// Parse plugins into xcaddy format
	plugins, err := parsePlugins(req.Plugins)
	if err != nil {
		b.logger.Error("failed to parse plugins",
			"error", err.Error(),
			"plugins", req.Plugins,
		)
		return nil, fmt.Errorf("failed to parse plugins: %w", err)
	}

	// Parse replacements into xcaddy format
	replacements, err := parseReplacements(req.Replacements)
	if err != nil {
		b.logger.Error("failed to parse replacements",
			"error", err.Error(),
			"replacements", req.Replacements,
		)
		return nil, fmt.Errorf("failed to parse replacements: %w", err)
	}

	// Create a temporary directory for this build
	buildDir, err := os.MkdirTemp(b.workDir, "build-*")
	if err != nil {
		b.logger.Error("failed to create build directory",
			"error", err.Error(),
			"work_dir", b.workDir,
		)
		return nil, fmt.Errorf("failed to create build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// Prepare xcaddy builder
	// Docker will capture stdout/stderr automatically
	builder := &xcaddy.Builder{
		CaddyVersion:    req.CaddyVersion,
		Plugins:         plugins,
		Replacements:    replacements,
		OS:              req.OS,
		Arch:            req.Arch,
		Cgo:             os.Getenv("CGO_ENABLED") == "1",
		RaceDetector:    false,
		SkipCleanup:     false,
		TimeoutGet:      5 * time.Minute,
		TimeoutBuild:    30 * time.Minute,
		AdditionalEnv:   b.additionalEnv,
		DockerHost:      b.dockerHost,
		DockerImage:     b.dockerImage,
		DockerMemoryMB:  b.dockerMemoryMB,
		DockerCPUs:      b.dockerCPUs,
		DockerPIDsLimit: b.dockerPIDsLimit,
	}

	// Set output path
	outputFile := filepath.Join(buildDir, "caddy")
	if req.OS == "windows" {
		outputFile += ".exe"
	}

	// Perform the build
	buildErr := builder.Build(ctx, outputFile, b.logger)

	// Check if build failed
	if buildErr != nil {
		b.logger.Error("build failed",
			"error", buildErr.Error(),
			"version", req.CaddyVersion,
			"os", req.OS,
			"arch", req.Arch,
			"plugins", req.Plugins,
			"replacements", req.Replacements,
		)
		return nil, fmt.Errorf("build failed: %w", buildErr)
	}

	// Sign the binary if signer is configured
	if b.signer != nil {
		b.logger.Info("signing binary",
			"build_key", req.BuildKey,
			"output_file", outputFile,
		)

		signature, err := b.signer.SignFile(outputFile)
		if err != nil {
			b.logger.Error("failed to sign binary",
				"error", err.Error(),
				"output_file", outputFile,
			)
			return nil, fmt.Errorf("failed to sign binary: %w", err)
		}

		// Upload signature to storage (append .asc to storage key)
		signatureKey := req.StorageKey + ".asc"
		b.logger.Info("uploading signature to storage",
			"storage_key", signatureKey,
			"signature_size", len(signature),
		)

		sigReader := bytes.NewReader(signature)
		if err := b.storage.Put(ctx, signatureKey, sigReader); err != nil {
			b.logger.Error("failed to upload signature to storage",
				"error", err.Error(),
				"storage_key", signatureKey,
			)
			return nil, fmt.Errorf("failed to upload signature to storage: %w", err)
		}

		b.logger.Info("signature uploaded successfully",
			"storage_key", signatureKey,
		)
	}

	// Upload the built binary to storage
	{
		b.logger.Info("uploading binary to storage",
			"storage_key", req.StorageKey,
			"output_file", outputFile,
		)

		file, err := os.Open(outputFile)
		if err != nil {
			b.logger.Error("failed to open built binary",
				"error", err.Error(),
				"output_file", outputFile,
			)
			return nil, fmt.Errorf("failed to open built binary: %w", err)
		}
		defer file.Close()

		if err := b.storage.Put(ctx, req.StorageKey, file); err != nil {
			b.logger.Error("failed to upload binary to storage",
				"error", err.Error(),
				"storage_key", req.StorageKey,
			)
			return nil, fmt.Errorf("failed to upload binary to storage: %w", err)
		}
	}

	// Log successful completion
	b.logger.Info("build and upload completed successfully",
		"version", req.CaddyVersion,
		"os", req.OS,
		"arch", req.Arch,
		"plugin_count", len(req.Plugins),
		"storage_key", req.StorageKey,
		"has_signature", b.signer != nil,
	)

	return &BuildResult{
		StorageKey:   req.StorageKey,
		HasSignature: b.signer != nil,
	}, nil
}

// parsePlugins converts plugin strings into xcaddy Plugin format
func parsePlugins(plugins []string) ([]xcaddy.Plugin, error) {
	var result []xcaddy.Plugin

	for _, plugin := range plugins {
		// Parse format: "module@version" or just "module"
		parts := strings.SplitN(plugin, "@", 2)
		modulePath := strings.TrimSpace(parts[0])

		if modulePath == "" {
			continue // Skip empty entries
		}

		p := xcaddy.Plugin{
			PackagePath: modulePath,
		}

		// If version is specified, use it
		if len(parts) == 2 && parts[1] != "" {
			p.Version = parts[1]
		}

		result = append(result, p)
	}

	return result, nil
}

// parseReplacements converts replacement strings into xcaddy Replacement format
func parseReplacements(replacements []string) ([]xcaddy.Replacement, error) {
	var result []xcaddy.Replacement

	for _, replacement := range replacements {
		// Parse format: "old=new"
		parts := strings.SplitN(replacement, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid replacement format: %s (expected 'old=new')", replacement)
		}

		old := strings.TrimSpace(parts[0])
		new := strings.TrimSpace(parts[1])

		if old == "" || new == "" {
			return nil, fmt.Errorf("invalid replacement format: %s (old and new cannot be empty)", replacement)
		}

		result = append(result, xcaddy.Replacement{
			Old: old,
			New: new,
		})
	}

	return result, nil
}
