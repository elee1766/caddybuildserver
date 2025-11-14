package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/builder"
	"github.com/elee1766/caddybuildserver/pkg/config"
	"github.com/elee1766/caddybuildserver/pkg/logger"
	"github.com/elee1766/caddybuildserver/pkg/preemption"
	"github.com/elee1766/caddybuildserver/pkg/preemption/azure"
	"github.com/elee1766/caddybuildserver/pkg/preemption/sigusr1"
	"github.com/elee1766/caddybuildserver/pkg/storage"
	"github.com/elee1766/caddybuildserver/pkg/worker"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func main() {
	// Ignore SIGPIPE to prevent crashes during shutdown when trying to write to closed pipes
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)

	fx.New(
		fx.Provide(
			config.Load,
			provideLogger,
			provideDatabase,
			provideStorage,
			provideBuilder,
			providePreemptor,
			provideWorker,
		),
		fx.WithLogger(func(rootLogger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: rootLogger}
		}),
		fx.Invoke(func(*worker.Worker) {}),
	).Run()
}

// provideLogger creates the root logger
func provideLogger() *slog.Logger {
	return logger.New()
}

// provideDatabase creates a database connection pool
func provideDatabase(lc fx.Lifecycle, cfg *config.Config, rootLogger *slog.Logger) (*pgxpool.Pool, error) {
	dbLogger := logger.With(rootLogger, "database")
	ctx := context.Background()

	dbLogger.Info("connecting to database")
	pool, err := pgxpool.New(ctx, cfg.Database.ConnectionString())
	if err != nil {
		dbLogger.Error("failed to create connection pool", "error", err)
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Test connection
	if err := pool.Ping(ctx); err != nil {
		dbLogger.Error("failed to ping database", "error", err)
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	dbLogger.Info("successfully connected to database")

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			pool.Close()
			return nil
		},
	})

	return pool, nil
}

// provideStorage creates a storage instance
func provideStorage(lc fx.Lifecycle, cfg *config.Config, rootLogger *slog.Logger) (*storage.Storage, error) {
	ctx := context.Background()
	storageLogger := logger.With(rootLogger, "storage")
	stor, err := storage.New(ctx, cfg.Storage, storageLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	return stor, nil
}

// provideBuilder creates a builder instance
func provideBuilder(cfg *config.Config, storage *storage.Storage, pool *pgxpool.Pool, rootLogger *slog.Logger) (*builder.Builder, error) {
	builderLogger := logger.With(rootLogger, "builder")

	// Log Docker configuration
	if cfg.Worker.DockerHost != "" {
		builderLogger.Info("using custom Docker host", "docker_host", cfg.Worker.DockerHost)
	} else {
		builderLogger.Info("using default Docker host from environment or default socket")
	}

	bldr, err := builder.New(
		cfg.Worker.BuildPath,
		storage,
		pool,
		cfg.Worker.BuildEnv,
		cfg.Worker.DockerHost,
		cfg.Worker.DockerImage,
		cfg.Worker.DockerMemoryMB,
		cfg.Worker.DockerCPUs,
		cfg.Worker.DockerPIDsLimit,
		cfg.Worker.Signing.Enabled,
		cfg.Worker.Signing.KeyFile,
		cfg.Worker.Signing.KeyPassword,
		builderLogger,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize builder: %w", err)
	}

	builderLogger.Info("builder initialized",
		"build_path", cfg.Worker.BuildPath,
		"docker_memory_mb", cfg.Worker.DockerMemoryMB,
		"docker_cpus", cfg.Worker.DockerCPUs,
		"docker_pids_limit", cfg.Worker.DockerPIDsLimit,
		"additional_env_vars", len(cfg.Worker.BuildEnv),
		"signing_enabled", cfg.Worker.Signing.Enabled,
	)

	return bldr, nil
}

// providePreemptor creates a preemptor if configured
func providePreemptor(cfg *config.Config, rootLogger *slog.Logger) preemption.Preemptor {
	if !cfg.Worker.Preemption.Enabled {
		return nil
	}

	preemptLogger := logger.With(rootLogger, "preemptor")
	pollInterval := time.Duration(cfg.Worker.Preemption.PollInterval) * time.Second
	if pollInterval == 0 {
		pollInterval = 5 * time.Second
	}

	switch cfg.Worker.Preemption.Provider {
	case "azure":
		preemptLogger.Info("azure preemptor enabled", "poll_interval", pollInterval)
		return azure.NewAzurePreemptor(pollInterval, preemptLogger)
	case "sigusr1":
		preemptLogger.Info("sigusr1 preemptor enabled")
		return sigusr1.NewSIGUSR1Preemptor(preemptLogger)
	default:
		preemptLogger.Warn("unknown preemption provider", "provider", cfg.Worker.Preemption.Provider)
		return nil
	}
}

// provideWorker creates and starts the worker
func provideWorker(lc fx.Lifecycle, shutdowner fx.Shutdowner, cfg *config.Config, pool *pgxpool.Pool, bldr *builder.Builder, preemptor preemption.Preemptor, rootLogger *slog.Logger) *worker.Worker {
	workerLogger := logger.With(rootLogger, "worker")

	// Log worker configuration
	workerLogger.Info("worker configuration",
		"max_jobs", cfg.Worker.MaxJobs,
		"build_path", cfg.Worker.BuildPath,
		"stop_timeout", cfg.Worker.StopTimeout,
		"docker_host", cfg.Worker.DockerHost,
		"preemption_enabled", cfg.Worker.Preemption.Enabled,
	)

	w := worker.New(pool, bldr, preemptor, cfg.Worker.MaxJobs, workerLogger)

	// Set the shutdowner so the worker can trigger shutdown after preemption
	w.SetShutdowner(func() error {
		workerLogger.Info("worker requesting application shutdown")
		return shutdowner.Shutdown()
	})

	// Create a context for the worker's lifetime
	ctx, cancel := context.WithCancel(context.Background())

	// Set stop timeout (default 5 minutes)
	stopTimeout := time.Duration(cfg.Worker.StopTimeout) * time.Second
	if stopTimeout == 0 {
		stopTimeout = 5 * time.Minute
	}

	lc.Append(fx.Hook{
		OnStart: func(startCtx context.Context) error {
			workerLogger.Info("starting worker", "max_jobs", cfg.Worker.MaxJobs)
			go w.Start(ctx)
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			workerLogger.Info("stopping worker...", "stop_timeout", stopTimeout)

			// Signal worker to stop
			w.Stop()
			cancel() // Cancel the worker's context

			// Wait for worker to finish with timeout
			select {
			case <-w.Stopped():
				workerLogger.Info("worker stopped gracefully")
				return nil
			case <-time.After(stopTimeout):
				workerLogger.Warn("worker stop timeout exceeded, forcing shutdown")
				return nil
			}
		},
	})

	return w
}
