package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"text/template"

	"github.com/goccy/go-yaml"
)

// Config represents the application configuration
type Config struct {
	Database DatabaseConfig `yaml:"database"`
	Storage  StorageConfig  `yaml:"storage"`
	API      APIConfig      `yaml:"api"`
	Worker   WorkerConfig   `yaml:"worker"`
}

// DatabaseConfig contains database connection settings
type DatabaseConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	SSLMode  string `yaml:"ssl_mode"`
}

// StorageConfig contains storage backend settings
type StorageConfig struct {
	Type   string       `yaml:"type"` // "local" or "s3"
	Local  LocalConfig  `yaml:"local,omitempty"`
	S3     S3Config     `yaml:"s3,omitempty"`
}

// LocalConfig contains local filesystem storage settings
type LocalConfig struct {
	Path string `yaml:"path"` // Directory path for local storage
}

// S3Config contains S3 storage settings
type S3Config struct {
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint,omitempty"`  // For S3-compatible services
	AccessKey string `yaml:"access_key,omitempty"`
	SecretKey string `yaml:"secret_key,omitempty"`
}

// APIConfig contains API server settings
type APIConfig struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	GoProxy string `yaml:"go_proxy"` // Go module proxy URL (default: https://proxy.golang.org)
}

// WorkerConfig contains worker settings
type WorkerConfig struct {
	MaxJobs         int               `yaml:"max_jobs"`          // Maximum concurrent jobs
	BuildPath       string            `yaml:"build_path"`        // Temporary directory for builds
	StopTimeout     int               `yaml:"stop_timeout"`      // Graceful shutdown timeout in seconds (default: 300)
	DockerHost      string            `yaml:"docker_host"`       // Docker socket path (e.g., "unix:///var/run/docker.sock")
	DockerImage     string            `yaml:"docker_image"`      // Docker image to use for builds (default: "golang:1.25")
	DockerMemoryMB  int64             `yaml:"docker_memory_mb"`  // Memory limit in MB (default: 2048)
	DockerCPUs      float64           `yaml:"docker_cpus"`       // CPU limit in cores (default: 2.0)
	DockerPIDsLimit int64             `yaml:"docker_pids_limit"` // PID limit (default: 100)
	BuildEnv        map[string]string `yaml:"build_env"`         // Additional environment variables to pass to build processes
	Preemption      PreemptionConfig  `yaml:"preemption"`        // Preemption monitoring settings
	Signing         SigningConfig     `yaml:"signing"`           // Binary signing settings
}

// SigningConfig contains binary signing settings
type SigningConfig struct {
	Enabled     bool   `yaml:"enabled"`      // Enable binary signing with PGP
	KeyFile     string `yaml:"key_file"`     // Path to ASCII-armored PGP signing key file
	KeyPassword string `yaml:"key_password"` // Password for encrypted signing key (supports template: {{ env "KEY_PASSWORD" }})
}

// PreemptionConfig contains preemption monitoring settings
type PreemptionConfig struct {
	Enabled      bool   `yaml:"enabled"`       // Enable preemption monitoring
	Provider     string `yaml:"provider"`      // Cloud provider: "azure", "aws", "gcp", or empty to disable
	PollInterval int    `yaml:"poll_interval"` // Poll interval in seconds (default: 5)
}

// ConnectionString returns the PostgreSQL connection string
func (d *DatabaseConfig) ConnectionString() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Database, d.SSLMode,
	)
}

// Load reads and parses the configuration file
func Load() (*Config, error) {
	// Get config file path from environment variable or use default
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "./config.yml"
	}

	// Read the config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Parse as Go template to allow environment variable substitution
	tmpl, err := template.New("config").Funcs(templateFuncs()).Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse config template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("failed to execute config template: %w", err)
	}

	// Parse YAML
	var config Config
	if err := yaml.Unmarshal(buf.Bytes(), &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Log parsed configuration (useful for debugging template substitution)
	slog.Info("config loaded",
		"config_path", configPath,
		"worker_docker_host", config.Worker.DockerHost,
		"env_docker_host", os.Getenv("DOCKER_HOST"),
	)

	return &config, nil
}

// templateFuncs returns template functions available in config files
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		// env returns the value of an environment variable
		// Usage: {{ env "DATABASE_HOST" }}
		"env": os.Getenv,

		// envOr returns the value of an environment variable or a default value
		// Usage: {{ envOr "DATABASE_HOST" "localhost" }}
		"envOr": func(key, defaultValue string) string {
			if v := os.Getenv(key); v != "" {
				return v
			}
			return defaultValue
		},
	}
}
