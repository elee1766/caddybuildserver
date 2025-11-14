package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/elee1766/caddybuildserver/pkg/config"
	s3afero "github.com/fclairamb/afero-s3"
	"github.com/spf13/afero"
)

// Storage provides an abstraction over different storage backends
type Storage struct {
	fs     afero.Fs
	logger *slog.Logger
}

// New creates a new storage instance based on the configuration
func New(ctx context.Context, cfg config.StorageConfig, logger *slog.Logger) (*Storage, error) {
	logger.Info("initializing storage", "type", cfg.Type)

	var fs afero.Fs

	switch cfg.Type {
	case "local":
		if cfg.Local.Path == "" {
			return nil, fmt.Errorf("local storage path is required")
		}
		logger.Info("using local filesystem storage", "path", cfg.Local.Path)
		fs = afero.NewBasePathFs(afero.NewOsFs(), cfg.Local.Path)

	case "s3":
		if cfg.S3.Bucket == "" {
			return nil, fmt.Errorf("s3 bucket is required")
		}
		if cfg.S3.Region == "" {
			return nil, fmt.Errorf("s3 region is required")
		}

		// Build AWS config
		awsCfg := &aws.Config{
			Region: aws.String(cfg.S3.Region),
		}

		// Use custom credentials if provided
		if cfg.S3.AccessKey != "" && cfg.S3.SecretKey != "" {
			awsCfg.Credentials = credentials.NewStaticCredentials(
				cfg.S3.AccessKey,
				cfg.S3.SecretKey,
				"",
			)
		}

		// Set custom endpoint if provided (for MinIO, etc.)
		if cfg.S3.Endpoint != "" {
			awsCfg.Endpoint = aws.String(cfg.S3.Endpoint)
			awsCfg.S3ForcePathStyle = aws.Bool(true) // Required for MinIO and some S3-compatible services
		}

		// Create AWS session
		sess, err := session.NewSession(awsCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create AWS session: %w", err)
		}

		// Create afero S3 filesystem
		fs = s3afero.NewFs(cfg.S3.Bucket, sess)
		logger.Info("using S3 storage", "bucket", cfg.S3.Bucket, "region", cfg.S3.Region)

	default:
		return nil, fmt.Errorf("unsupported storage type: %s", cfg.Type)
	}

	return &Storage{
		fs:     fs,
		logger: logger,
	}, nil
}

// Put stores a file with the given key
func (s *Storage) Put(ctx context.Context, key string, reader io.Reader) error {
	// Ensure directory exists
	dir := filepath.Dir(key)
	if dir != "." && dir != "/" {
		if err := s.fs.MkdirAll(dir, 0755); err != nil {
			s.logger.Error("failed to create directory", "key", key, "error", err)
			return fmt.Errorf("failed to create directory: %w", err)
		}
	}

	// Create the file
	file, err := s.fs.Create(key)
	if err != nil {
		s.logger.Error("failed to create file", "key", key, "error", err)
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// Copy data
	if _, err := io.Copy(file, reader); err != nil {
		s.logger.Error("failed to write file", "key", key, "error", err)
		return fmt.Errorf("failed to write file: %w", err)
	}

	s.logger.Info("file stored successfully", "key", key)
	return nil
}

// Get retrieves a file by key
func (s *Storage) Get(ctx context.Context, key string) (afero.File, error) {
	file, err := s.fs.Open(key)
	if err != nil {
		s.logger.Error("failed to open file", "key", key, "error", err)
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	s.logger.Info("file retrieved successfully", "key", key)
	return file, nil
}

// Exists checks if a file exists
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := afero.Exists(s.fs, key)
	if err != nil {
		return false, fmt.Errorf("failed to check file existence: %w", err)
	}
	return exists, nil
}

// Delete removes a file by key
func (s *Storage) Delete(ctx context.Context, key string) error {
	if err := s.fs.Remove(key); err != nil {
		s.logger.Error("failed to delete file", "key", key, "error", err)
		return fmt.Errorf("failed to delete file: %w", err)
	}
	s.logger.Info("file deleted successfully", "key", key)
	return nil
}
