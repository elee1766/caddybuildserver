package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elee1766/caddybuildserver/migrations"
	"github.com/elee1766/caddybuildserver/pkg/config"
	"github.com/elee1766/caddybuildserver/pkg/janitor"
	"github.com/elee1766/caddybuildserver/pkg/logger"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
)

func main() {
	fx.New(
		fx.Provide(
			config.Load,
			provideLogger,
			provideDatabase,
			provideJanitor,
		),
		fx.WithLogger(func(rootLogger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: rootLogger}
		}),
		fx.Invoke(func(*janitor.Janitor) {}),
		fx.StopTimeout(30*time.Second), // Allow rescue cycle to complete (runs every 30s)
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

// provideJanitor creates and starts the janitor
func provideJanitor(lc fx.Lifecycle, pool *pgxpool.Pool, rootLogger *slog.Logger) (*janitor.Janitor, error) {
	janitorLogger := logger.With(rootLogger, "janitor")

	// Create janitor
	j, err := janitor.New(pool, janitorLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to create janitor: %w", err)
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			// Run database migrations first
			janitorLogger.Info("running database migrations")
			if err := migrations.Run(ctx, pool, janitorLogger); err != nil {
				janitorLogger.Error("failed to run migrations", "error", err)
				return fmt.Errorf("failed to run migrations: %w", err)
			}

			// Start the janitor scheduler
			if err := j.Start(); err != nil {
				return fmt.Errorf("failed to start janitor: %w", err)
			}

			return nil
		},
		OnStop: func(ctx context.Context) error {
			return j.Stop(ctx)
		},
	})

	return j, nil
}
