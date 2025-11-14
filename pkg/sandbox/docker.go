package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// DockerConfig defines the Docker-based sandbox configuration for build isolation
type DockerConfig struct {
	// Image is the Docker image to use for builds (e.g., "golang:1.21-alpine")
	Image string

	// WorkDir is the working directory inside the container
	WorkDir string

	// Mounts are directory mappings from host to container
	Mounts []mount.Mount

	// Environment variables to set (only these will be available)
	Env map[string]string

	// AllowNetwork allows network access in the container
	AllowNetwork bool

	// MemoryLimit in bytes (e.g., 2 * 1024 * 1024 * 1024 for 2GB)
	MemoryLimit int64

	// CPUQuota is the CPU quota in microseconds per 100ms period
	// e.g., 200000 = 2 CPUs (200000/100000)
	CPUQuota int64

	// PidsLimit is the maximum number of processes
	PidsLimit int64

	// User to run as inside container (e.g., "1000:1000")
	User string

	// DockerHost is the Docker daemon socket (e.g., "unix:///var/run/docker.sock")
	// If empty, uses DOCKER_HOST env var or default
	DockerHost string
}

// DefaultDockerConfig returns a secure default Docker configuration for Go builds
func DefaultDockerConfig() *DockerConfig {
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/tmp/home"
	}

	return &DockerConfig{
		Image:        "golang:1.25",
		WorkDir:      "/workspace",
		AllowNetwork: false,                  // Disabled by default for security, enabled per-command as needed
		MemoryLimit:  2 * 1024 * 1024 * 1024, // 2GB
		CPUQuota:     200000,                 // 2 CPUs
		PidsLimit:    100,
		Env: map[string]string{
			"CGO_ENABLED": "0", // Disable CGO by default for security
			"GOCACHE":     "/go-cache",
			"GOMODCACHE":  "/go-mod-cache",
		},
		// Go build caches and modules caches - persisted for performance, but this could possibly be insecure. im not sure.
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeBind,
				Source: filepath.Join(homeDir, ".cache/go-build"),
				Target: "/go-cache",
			},
			{
				Type:   mount.TypeBind,
				Source: filepath.Join(homeDir, "go/pkg/mod"),
				Target: "/go-mod-cache",
			},
		},
	}
}

// DockerSandbox manages Docker-based sandboxed execution
type DockerSandbox struct {
	client *client.Client
	config *DockerConfig
}

// NewDockerSandbox creates a new Docker sandbox with the given configuration
func NewDockerSandbox(config *DockerConfig) (*DockerSandbox, error) {
	if config == nil {
		config = DefaultDockerConfig()
	}

	// Create Docker client options
	var opts []client.Opt

	// Set Docker host - use custom if specified, otherwise FromEnv
	if config.DockerHost != "" {
		// Use custom host and manually set TLS config from environment if needed
		opts = []client.Opt{
			client.WithHost(config.DockerHost),
			client.WithAPIVersionNegotiation(),
		}
		// Check if we need TLS (for tcp:// connections)
		if tlsVerify := os.Getenv("DOCKER_TLS_VERIFY"); tlsVerify != "" {
			opts = append(opts, client.FromEnv) // This will add TLS config
		}
	} else {
		// Use environment settings (DOCKER_HOST, DOCKER_TLS_VERIFY, etc.)
		opts = []client.Opt{
			client.FromEnv,
			client.WithAPIVersionNegotiation(),
		}
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	return &DockerSandbox{
		client: cli,
		config: config,
	}, nil
}

// Close closes the Docker client connection
func (d *DockerSandbox) Close() error {
	return d.client.Close()
}

// ensureCacheDirs ensures the cache directories exist on the host
func (d *DockerSandbox) ensureCacheDirs() error {
	for _, mnt := range d.config.Mounts {
		if mnt.Type == mount.TypeBind && mnt.Source != "" {
			if err := os.MkdirAll(mnt.Source, 0755); err != nil {
				return fmt.Errorf("failed to create cache directory %s: %w", mnt.Source, err)
			}
		}
	}
	return nil
}

// PullImage pulls the Docker image if it doesn't exist locally
func (d *DockerSandbox) PullImage(ctx context.Context) error {
	// Check if image exists locally
	_, _, err := d.client.ImageInspectWithRaw(ctx, d.config.Image)
	if err == nil {
		// Image already exists
		return nil
	}

	// Pull the image
	reader, err := d.client.ImagePull(ctx, d.config.Image, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %w", d.config.Image, err)
	}
	defer reader.Close()

	// Consume the output to ensure pull completes
	_, err = io.Copy(io.Discard, reader)
	return err
}

// RunCommand runs a command inside a Docker container with full isolation
// Network access is determined by the DockerConfig.AllowNetwork setting
func (d *DockerSandbox) RunCommand(ctx context.Context, name string, args []string, workDir string, stdout, stderr io.Writer) error {
	return d.RunCommandWithNetwork(ctx, name, args, workDir, stdout, stderr, d.config.AllowNetwork)
}

// RunCommandWithNetwork runs a command inside a Docker container with full isolation
// Allows overriding the network setting for this specific command
func (d *DockerSandbox) RunCommandWithNetwork(ctx context.Context, name string, args []string, workDir string, stdout, stderr io.Writer, allowNetwork bool) error {
	if err := d.ensureCacheDirs(); err != nil {
		return err
	}

	// Ensure working directory exists
	if err := os.MkdirAll(workDir, 0755); err != nil {
		return fmt.Errorf("failed to create working directory: %w", err)
	}

	// Add working directory as a mount
	mounts := append([]mount.Mount{}, d.config.Mounts...)
	mounts = append(mounts, mount.Mount{
		Type:   mount.TypeBind,
		Source: workDir,
		Target: d.config.WorkDir,
	})

	// Build command
	cmd := append([]string{name}, args...)

	// Build environment variables
	env := make([]string, 0, len(d.config.Env))
	for key, value := range d.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}

	// Create container configuration
	containerConfig := &container.Config{
		Image:      d.config.Image,
		Cmd:        cmd,
		WorkingDir: d.config.WorkDir,
		Env:        env,
		User:       d.config.User,
	}

	// Create host configuration with resource limits and security settings
	hostConfig := &container.HostConfig{
		Mounts: mounts,
		Resources: container.Resources{
			Memory:    d.config.MemoryLimit,
			CPUQuota:  d.config.CPUQuota,
			PidsLimit: &d.config.PidsLimit,
		},
		SecurityOpt: []string{
			"no-new-privileges", // Prevent privilege escalation
		},
		CapDrop:    []string{"ALL"}, // Drop all capabilities
		AutoRemove: true,            // Automatically remove container when it exits
	}

	// Network isolation
	networkMode := "default"
	if !allowNetwork {
		networkMode = "none"
	}
	hostConfig.NetworkMode = container.NetworkMode(networkMode)

	// Create the container
	resp, err := d.client.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}
	containerID := resp.ID

	// Ensure container is removed if we exit early
	defer func() {
		// Try to remove container, ignore errors since AutoRemove might have already done it
		_ = d.client.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
	}()

	// Attach to container to get output
	attachResp, err := d.client.ContainerAttach(ctx, containerID, container.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("failed to attach to container: %w", err)
	}
	defer attachResp.Close()

	// Start the container
	if err := d.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Stream output in a goroutine
	outputErr := make(chan error, 1)
	go func() {
		// Docker uses multiplexed streams, use stdcopy to demux
		_, err := stdcopy.StdCopy(stdout, stderr, attachResp.Reader)
		outputErr <- err
	}()

	// Wait for container to finish
	statusCh, errCh := d.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return fmt.Errorf("container wait error: %w", err)
	case status := <-statusCh:
		// Wait for output streaming to complete
		<-outputErr

		if status.StatusCode != 0 {
			return fmt.Errorf("container exited with status code %d", status.StatusCode)
		}
	}

	return nil
}

// AddEnv adds an environment variable to the configuration
func (d *DockerSandbox) AddEnv(key, value string) {
	if d.config.Env == nil {
		d.config.Env = make(map[string]string)
	}
	d.config.Env[key] = value
}
