package janitor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/db"
	"github.com/go-co-op/gocron/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Janitor is the coordinator that does maintenance work on the database.
type Janitor struct {
	pool      *pgxpool.Pool
	logger    *slog.Logger
	scheduler gocron.Scheduler
}

// New creates a new Janitor instance with a scheduler
func New(pool *pgxpool.Pool, logger *slog.Logger) (*Janitor, error) {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		return nil, fmt.Errorf("failed to create scheduler: %w", err)
	}

	return &Janitor{
		pool:      pool,
		logger:    logger,
		scheduler: scheduler,
	}, nil
}

// Start begins the janitor's scheduled tasks
func (j *Janitor) Start() error {
	// Schedule rescue cycle to run every 10 seconds
	_, err := j.scheduler.NewJob(
		gocron.DurationJob(10*time.Second),
		gocron.NewTask(j.rescueCycle),
		gocron.WithName("rescue-stuck-builds"),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule rescue cycle: %w", err)
	}

	// Schedule retry cycle to run every 10 seconds
	_, err = j.scheduler.NewJob(
		gocron.DurationJob(10*time.Second),
		gocron.NewTask(j.retryCycle),
		gocron.WithName("retry-failed-builds"),
		gocron.WithStartAt(gocron.WithStartImmediately()),
	)
	if err != nil {
		return fmt.Errorf("failed to schedule retry cycle: %w", err)
	}

	// Start the scheduler
	j.scheduler.Start()
	j.logger.Info("janitor scheduler started")

	return nil
}

// Stop gracefully shuts down the janitor
func (j *Janitor) Stop(ctx context.Context) error {
	j.logger.Info("shutting down janitor...")

	// Stop accepting new jobs immediately
	j.scheduler.StopJobs()

	// Create a context with timeout for scheduler shutdown
	// This ensures we don't wait forever for jobs to complete
	shutdownCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// Shutdown the scheduler - this waits for running jobs to complete
	// or until the context times out
	done := make(chan error, 1)
	go func() {
		done <- j.scheduler.Shutdown()
	}()

	select {
	case err := <-done:
		if err != nil {
			j.logger.Error("error shutting down scheduler", "error", err)
			return err
		}
		j.logger.Info("janitor stopped")
		return nil
	case <-shutdownCtx.Done():
		j.logger.Warn("janitor shutdown timed out, forcing stop")
		return shutdownCtx.Err()
	}
}

// rescueCycle performs one cycle of rescuing stuck builds
func (j *Janitor) rescueCycle() {
	ctx := context.Background()

	// Rescue builds that haven't heartbeated in 1 minute
	stuckThreshold := 1 * time.Minute

	rescuedCount, err := db.RescueStuckBuilds(ctx, j.pool, stuckThreshold)
	if err != nil {
		j.logger.Error("failed to rescue stuck builds", "error", err)
		return
	}

	if rescuedCount > 0 {
		j.logger.Info("rescued stuck builds",
			"count", rescuedCount,
			"threshold", stuckThreshold,
		)
	}
}

// retryCycle performs one cycle of retrying failed builds with attempts remaining
func (j *Janitor) retryCycle() {
	ctx := context.Background()

	retriedCount, err := db.RetryFailedBuilds(ctx, j.pool)
	if err != nil {
		j.logger.Error("failed to retry failed builds", "error", err)
		return
	}

	if retriedCount > 0 {
		j.logger.Info("retrying failed builds", "count", retriedCount)
	}
}
