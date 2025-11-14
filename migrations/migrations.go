package migrations

import (
	"context"
	"embed"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed *.sql
var migrationFiles embed.FS

// Run runs all pending database migrations
func Run(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	logger.Info("running database migrations")

	// Acquire a connection from the pool
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire connection: %w", err)
	}
	defer conn.Release()

	// Create migrator
	migrator, err := migrate.NewMigrator(ctx, conn.Conn(), "schema_version")
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	// Load migrations from embedded filesystem - tern handles parsing the files
	if err := migrator.LoadMigrations(migrationFiles); err != nil {
		return fmt.Errorf("failed to load migrations: %w", err)
	}

	// Get current version
	logger.Info("getting migration current version")
	currentVersion, err := migrator.GetCurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to get current version: %w", err)
	}

	logger.Info("running migrations", "startVersion", currentVersion)

	// Run migrations
	err = migrator.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}

	// Get new version
	currentVersion, err = migrator.GetCurrentVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to get new version: %w", err)
	}

	logger.Info("migrations completed", "endVersion", currentVersion)

	return nil
}
