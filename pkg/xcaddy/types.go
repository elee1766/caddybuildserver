package xcaddy

import (
	"io"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/sandbox"
)

// Builder can produce a custom Caddy build with the configuration it represents.
type Builder struct {
	// CaddyVersion is the version of Caddy to build (e.g., "v2.7.6")
	CaddyVersion string

	// Plugins is the list of plugins to include in the build
	Plugins []Plugin

	// Replacements is the list of Go module replacements
	Replacements []Replacement

	// OS is the target operating system (e.g., "linux", "darwin", "windows")
	OS string

	// Arch is the target architecture (e.g., "amd64", "arm64")
	Arch string

	// ARM specifies the ARM version (e.g., "5", "6", "7") - only used when Arch is "arm"
	ARM string

	// Cgo enables CGO for the build
	Cgo bool

	// RaceDetector enables the Go race detector (forces CGO=1)
	RaceDetector bool

	// Debug enables debug symbols in the binary
	Debug bool

	// TimeoutGet is the timeout for go get operations
	TimeoutGet time.Duration

	// TimeoutBuild is the timeout for the entire build operation
	TimeoutBuild time.Duration

	// SkipCleanup prevents deletion of the temporary build folder
	SkipCleanup bool

	// Stdout is the writer for process stdout (optional, defaults to os.Stdout)
	Stdout io.Writer

	// Stderr is the writer for process stderr (optional, defaults to os.Stderr)
	Stderr io.Writer

	// AdditionalEnv contains additional environment variables to pass to build processes
	// These are added on top of the minimal required environment variables
	AdditionalEnv map[string]string

	// DockerHost is the Docker daemon socket (e.g., "unix:///var/run/docker.sock")
	// If empty, uses DOCKER_HOST env var or default
	DockerHost string

	// DockerImage is the Docker image to use for builds (e.g., "golang:1.25")
	// If empty, uses default from sandbox config
	DockerImage string

	// DockerMemoryMB is the memory limit in MB for Docker containers
	// If 0, uses default (2048 MB)
	DockerMemoryMB int64

	// DockerCPUs is the CPU limit in cores for Docker containers
	// If 0, uses default (2.0 cores)
	DockerCPUs float64

	// DockerPIDsLimit is the PID limit for Docker containers
	// If 0, uses default (100)
	DockerPIDsLimit int64

	// Sandbox enables sandboxed builds using Docker for isolation
	// If nil, builds run directly on the host (INSECURE for untrusted code)
	Sandbox *sandbox.DockerSandbox
}

// Plugin represents a Caddy plugin dependency.
type Plugin struct {
	// PackagePath is the Go module path (e.g., "github.com/caddy-dns/cloudflare")
	PackagePath string

	// Version is the version to use (e.g., "v0.0.1", or empty for latest)
	Version string
}

// String returns the plugin in the format "package@version" or just "package" if no version.
func (p Plugin) String() string {
	if p.Version != "" {
		return p.PackagePath + "@" + p.Version
	}
	return p.PackagePath
}

// Replacement represents a Go module replacement directive.
type Replacement struct {
	// Old is the module path to replace
	Old string

	// New is the replacement module path (can be a local path or another module)
	New string
}

// String returns the replacement in the format "old=new".
func (r Replacement) String() string {
	return r.Old + "=" + r.New
}
