package db

import (
	"context"
	"fmt"
	"time"

	"github.com/georgysavva/scany/v2/pgxscan"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execer can execute SQL
type Execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Querier can query SQL
type Querier = pgxscan.Querier

// Batcher can send batch queries
type Batcher interface {
	SendBatch(ctx context.Context, b *pgx.Batch) (br pgx.BatchResults)
}

// ExecQuerier can execute and query
type ExecQuerier interface {
	Execer
	Querier
}

// ExecBatchQuerier can execute, batch, and query
type ExecBatchQuerier interface {
	Execer
	Batcher
	Querier
}

// CreateBuild creates a new build job or returns existing one
func CreateBuild(ctx context.Context, db ExecQuerier, config BuildConfig) (*Build, error) {
	buildKey, err := config.Hash()
	if err != nil {
		return nil, fmt.Errorf("failed to generate build key: %w", err)
	}

	// Insert or get existing build job
	query := `
		INSERT INTO build_jobs (build_key, caddy_version, plugins, replacements, os, arch, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())
		ON CONFLICT (build_key) DO NOTHING
	`

	_, err = db.Exec(ctx, query,
		buildKey,
		config.CaddyVersion,
		config.Plugins,
		config.Replacements,
		config.OS,
		config.Arch,
		BuildStatusPending,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to insert build job: %w", err)
	}

	// Get the build job with current attempt count
	return GetBuildByKey(ctx, db, buildKey)
}

// GetBuildByKey retrieves a build by its key, combining job and latest attempt info
func GetBuildByKey(ctx context.Context, db Querier, buildKey string) (*Build, error) {
	query := `
		SELECT
			t.build_key,
			t.caddy_version,
			t.plugins,
			t.replacements,
			t.os,
			t.arch,
			t.status,
			t.storage_key,
			t.has_signature,
			t.current_worker_id as worker_id,
			t.locked_at,
			t.heartbeat_at,
			t.max_attempts,
			t.created_at,
			t.updated_at,
			t.completed_at,
			COALESCE(MAX(a.attempt_number), 0) as attempt,
			(SELECT error_message FROM build_attempts
			 WHERE build_key = t.build_key
			 ORDER BY attempt_number DESC LIMIT 1) as error_message
		FROM build_jobs t
		LEFT JOIN build_attempts a ON t.build_key = a.build_key
		WHERE t.build_key = $1
		GROUP BY t.build_key, t.caddy_version, t.plugins, t.replacements, t.os, t.arch,
		         t.status, t.storage_key, t.has_signature, t.current_worker_id, t.locked_at, t.heartbeat_at,
		         t.max_attempts, t.created_at, t.updated_at, t.completed_at
	`

	var build Build
	err := pgxscan.Get(ctx, db, &build, query, buildKey)
	if err != nil {
		if pgxscan.NotFound(err) {
			return nil, fmt.Errorf("build not found")
		}
		return nil, fmt.Errorf("failed to get build: %w", err)
	}

	return &build, nil
}

// ClaimNextBuild claims the next pending build and creates a new attempt record
func ClaimNextBuild(ctx context.Context, db ExecQuerier, workerID string) (*Build, error) {
	// Start a transaction-like operation using CTE
	query := `
		WITH next_build AS (
			SELECT build_key
			FROM build_jobs
			WHERE status = 'pending'
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		),
		updated_target AS (
			UPDATE build_jobs t
			SET
				status = 'building'::build_status,
				current_worker_id = $1,
				locked_at = NOW(),
				heartbeat_at = NOW(),
				updated_at = NOW()
			FROM next_build nb
			WHERE t.build_key = nb.build_key
			RETURNING t.*
		),
		new_attempt AS (
			INSERT INTO build_attempts (build_key, attempt_number, worker_id, status, started_at)
			SELECT
				ut.build_key,
				COALESCE((SELECT MAX(attempt_number) + 1 FROM build_attempts WHERE build_key = ut.build_key), 1),
				$1,
				'building'::build_status,
				NOW()
			FROM updated_target ut
			RETURNING *
		)
		SELECT
			t.build_key,
			t.caddy_version,
			t.plugins,
			t.replacements,
			t.os,
			t.arch,
			t.status,
			t.storage_key,
			t.has_signature,
			t.current_worker_id as worker_id,
			t.locked_at,
			t.heartbeat_at,
			t.max_attempts,
			t.created_at,
			t.updated_at,
			t.completed_at,
			a.attempt_number as attempt,
			NULL::text as error_message
		FROM updated_target t
		JOIN new_attempt a ON t.build_key = a.build_key
	`

	var build Build
	err := pgxscan.Get(ctx, db, &build, query, workerID)
	if err != nil {
		if pgxscan.NotFound(err) {
			return nil, nil // No jobs available
		}
		return nil, fmt.Errorf("failed to claim build: %w", err)
	}

	return &build, nil
}

// UpdateBuildStatus updates the status of a build job and completes the current attempt
func UpdateBuildStatus(ctx context.Context, db Execer, buildKey string, status BuildStatus, errorMessage *string, storageKey *string, hasSignature bool) error {
	now := time.Now()

	// Update the attempt record
	updateAttemptQuery := `
		UPDATE build_attempts
		SET status = $1, error_message = $2, completed_at = $3
		WHERE build_key = $4 AND status = 'building'::build_status
	`

	_, err := db.Exec(ctx, updateAttemptQuery, status, errorMessage, now, buildKey)
	if err != nil {
		return fmt.Errorf("failed to update build attempt: %w", err)
	}

	// Update the job
	var completedAt *time.Time
	if status == BuildStatusCompleted {
		completedAt = &now
	}

	updateJobQuery := `
		UPDATE build_jobs
		SET status = $1,
		    storage_key = $2,
		    has_signature = $3,
		    updated_at = $4,
		    completed_at = $5,
		    current_worker_id = NULL,
		    locked_at = NULL,
		    heartbeat_at = NULL
		WHERE build_key = $6
	`

	result, err := db.Exec(ctx, updateJobQuery, status, storageKey, hasSignature, now, completedAt, buildKey)
	if err != nil {
		return fmt.Errorf("failed to update build job: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("build job not found")
	}

	return nil
}

// UpdateHeartbeat updates the heartbeat timestamp for a build
func UpdateHeartbeat(ctx context.Context, db Execer, buildKey string, workerID string) error {
	query := `
		UPDATE build_jobs
		SET heartbeat_at = NOW(), updated_at = NOW()
		WHERE build_key = $1 AND current_worker_id = $2 AND status = 'building'::build_status
	`

	result, err := db.Exec(ctx, query, buildKey, workerID)
	if err != nil {
		return fmt.Errorf("failed to update heartbeat: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("build not found or not owned by worker")
	}

	return nil
}

// ReleaseJob releases a job back to pending status and marks the attempt as cancelled
func ReleaseJob(ctx context.Context, db Execer, buildKey string, workerID string) error {
	now := time.Now()

	// Mark the attempt as cancelled
	updateAttemptQuery := `
		UPDATE build_attempts
		SET status = 'failed'::build_status,
		    error_message = 'Job released due to preemption',
		    completed_at = $1
		WHERE build_key = $2 AND worker_id = $3 AND status = 'building'::build_status
	`

	_, err := db.Exec(ctx, updateAttemptQuery, now, buildKey, workerID)
	if err != nil {
		return fmt.Errorf("failed to update attempt: %w", err)
	}

	// Reset the job to pending
	updateJobQuery := `
		UPDATE build_jobs
		SET status = 'pending'::build_status,
		    current_worker_id = NULL,
		    locked_at = NULL,
		    heartbeat_at = NULL,
		    updated_at = $1
		WHERE build_key = $2 AND current_worker_id = $3 AND status = 'building'::build_status
	`

	result, err := db.Exec(ctx, updateJobQuery, now, buildKey, workerID)
	if err != nil {
		return fmt.Errorf("failed to release job: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("build not found or not owned by worker")
	}

	return nil
}

// RescueStuckBuilds finds builds that haven't heartbeated recently and resets them to pending
func RescueStuckBuilds(ctx context.Context, db Execer, stuckThreshold time.Duration) (int, error) {
	cutoff := time.Now().Add(-stuckThreshold)
	now := time.Now()

	// Mark stuck attempts as failed
	updateAttemptQuery := `
		UPDATE build_attempts a
		SET status = 'failed'::build_status,
		    error_message = 'Build rescued due to missed heartbeat',
		    completed_at = $1
		FROM build_jobs t
		WHERE a.build_key = t.build_key
		  AND a.status = 'building'::build_status
		  AND t.status = 'building'::build_status
		  AND t.heartbeat_at < $2
	`

	_, err := db.Exec(ctx, updateAttemptQuery, now, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to update stuck attempts: %w", err)
	}

	// Reset stuck jobs to pending
	updateJobQuery := `
		UPDATE build_jobs
		SET status = 'pending'::build_status,
		    current_worker_id = NULL,
		    locked_at = NULL,
		    heartbeat_at = NULL,
		    updated_at = $1
		WHERE status = 'building'::build_status
		  AND heartbeat_at < $2
	`

	result, err := db.Exec(ctx, updateJobQuery, now, cutoff)
	if err != nil {
		return 0, fmt.Errorf("failed to rescue stuck builds: %w", err)
	}

	return int(result.RowsAffected()), nil
}

// RetryFailedBuilds resets failed builds to pending if they have attempts remaining
// This is called by the janitor on an interval
func RetryFailedBuilds(ctx context.Context, db Execer) (int, error) {
	now := time.Now()

	query := `
		UPDATE build_jobs t
		SET status = 'pending'::build_status,
		    current_worker_id = NULL,
		    locked_at = NULL,
		    heartbeat_at = NULL,
		    updated_at = $1
		WHERE status = 'failed'::build_status
		  AND (SELECT COUNT(*) FROM build_attempts WHERE build_key = t.build_key) < t.max_attempts
	`

	result, err := db.Exec(ctx, query, now)
	if err != nil {
		return 0, fmt.Errorf("failed to retry failed builds: %w", err)
	}

	return int(result.RowsAffected()), nil
}

// ResetBuildAttempts resets a failed build back to pending with 0 attempts (force rebuild)
func ResetBuildAttempts(ctx context.Context, db Execer, buildKey string) error {
	now := time.Now()

	query := `
		UPDATE build_jobs
		SET status = 'pending'::build_status,
		    current_worker_id = NULL,
		    locked_at = NULL,
		    heartbeat_at = NULL,
		    completed_at = NULL,
		    updated_at = $1
		WHERE build_key = $2
	`

	result, err := db.Exec(ctx, query, now, buildKey)
	if err != nil {
		return fmt.Errorf("failed to reset build attempts: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("build not found")
	}

	return nil
}
