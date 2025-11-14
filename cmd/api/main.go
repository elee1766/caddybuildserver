package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/api"
	"github.com/elee1766/caddybuildserver/pkg/config"
	"github.com/elee1766/caddybuildserver/pkg/logger"
	"github.com/elee1766/caddybuildserver/pkg/modversion"
	"github.com/elee1766/caddybuildserver/pkg/storage"
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
			provideStorage,
			provideModResolver,
			provideAPIServer,
			provideHTTPServer,
		),
		fx.WithLogger(func(rootLogger *slog.Logger) fxevent.Logger {
			return &fxevent.SlogLogger{Logger: rootLogger}
		}),
		fx.Invoke(func(*http.Server) {}),
		fx.StopTimeout(10*time.Second), // Allow time for in-flight requests
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
	storageLogger := logger.With(rootLogger, "storage")
	ctx := context.Background()
	stor, err := storage.New(ctx, cfg.Storage, storageLogger)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	return stor, nil
}

// provideModResolver creates a module version resolver
func provideModResolver(cfg *config.Config) *modversion.Resolver {
	return modversion.NewResolver(cfg.API.GoProxy)
}

// provideAPIServer creates the API server with a scoped logger
func provideAPIServer(
	pool *pgxpool.Pool,
	stor *storage.Storage,
	resolver *modversion.Resolver,
	rootLogger *slog.Logger,
) *api.Server {
	apiLogger := logger.With(rootLogger, "api")
	return api.New(pool, stor, resolver, apiLogger)
}

// provideHTTPServer creates and starts the HTTP server
func provideHTTPServer(lc fx.Lifecycle, cfg *config.Config, apiServer *api.Server) *http.Server {
	addr := fmt.Sprintf("%s:%d", cfg.API.Host, cfg.API.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      apiServer,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				fmt.Printf("API server listening on %s\n", addr)
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					fmt.Printf("Server error: %v\n", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			fmt.Println("Shutting down server...")
			shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		},
	})

	return srv
}
