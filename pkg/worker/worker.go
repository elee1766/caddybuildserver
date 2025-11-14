package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/builder"
	"github.com/elee1766/caddybuildserver/pkg/db"
	"github.com/elee1766/caddybuildserver/pkg/preemption"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// jobTracker manages the state of active jobs
type jobTracker struct {
	inProgress   chan struct{}                 // Semaphore for max concurrent jobs
	active       map[string]context.CancelFunc // Tracks active jobs for cancellation
	activeMu     sync.RWMutex                  // Protects active map
	shuttingDown bool                          // Flag to stop accepting new jobs
	shutdownMu   sync.RWMutex                  // Protects shuttingDown flag
}

// addActiveJob adds a job to the active jobs map
func (j *jobTracker) addActiveJob(buildKey string, cancel context.CancelFunc) {
	j.activeMu.Lock()
	defer j.activeMu.Unlock()
	j.active[buildKey] = cancel
}

// removeActiveJob removes a job from the active jobs map
func (j *jobTracker) removeActiveJob(buildKey string) {
	j.activeMu.Lock()
	defer j.activeMu.Unlock()
	delete(j.active, buildKey)
}

// cancelAllActiveJobs cancels all active jobs and releases them back to the queue
func (j *jobTracker) cancelAllActiveJobs(ctx context.Context, pool *pgxpool.Pool, workerID string, logger *slog.Logger) {
	j.activeMu.RLock()
	defer j.activeMu.RUnlock()

	for buildKey, cancelFunc := range j.active {
		logger.Info("releasing job back to queue", "build_key", buildKey)

		// Release the job back to pending status
		if err := db.ReleaseJob(ctx, pool, buildKey, workerID); err != nil {
			logger.Error("failed to release job", "build_key", buildKey, "error", err)
		}

		// Cancel the job context
		cancelFunc()
	}
}

// Worker processes build jobs from the database queue
type Worker struct {
	workerID      string
	pool          *pgxpool.Pool
	builder       *builder.Builder
	preemptor     preemption.Preemptor
	logger        *slog.Logger
	maxConcurrent int
	pollInterval  time.Duration
	heartbeatInt  time.Duration
	done          chan struct{}
	stopped       chan struct{} // Closed when worker has fully stopped
	preempted     chan struct{}
	jobs          *jobTracker   // Job tracking state
	shutdowner    func() error  // Callback to shutdown the application after preemption
}

// New creates a new worker
func New(pool *pgxpool.Pool, bldr *builder.Builder, preemptor preemption.Preemptor, maxConcurrent int, logger *slog.Logger) *Worker {
	return &Worker{
		workerID:      generateWorkerID(),
		pool:          pool,
		builder:       bldr,
		preemptor:     preemptor,
		logger:        logger,
		maxConcurrent: maxConcurrent,
		pollInterval:  1 * time.Second,
		heartbeatInt:  30 * time.Second,
		done:          make(chan struct{}),
		stopped:       make(chan struct{}),
		preempted:     make(chan struct{}),
		shutdowner:    nil, // Set by SetShutdowner
		jobs: &jobTracker{
			inProgress: make(chan struct{}, maxConcurrent),
			active:     make(map[string]context.CancelFunc),
		},
	}
}

// SetShutdowner sets the shutdown callback for the worker
// This is called after preemption to gracefully shut down the application
func (w *Worker) SetShutdowner(shutdowner func() error) {
	w.shutdowner = shutdowner
}

// generateWorkerID creates a unique worker ID with sanitized hostname
func generateWorkerID() string {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	// Sanitize hostname: lowercase, replace non-alphanumeric with hyphens
	sanitized := strings.ToLower(hostname)
	reg := regexp.MustCompile(`[^a-z0-9]+`)
	sanitized = reg.ReplaceAllString(sanitized, "-")
	sanitized = strings.Trim(sanitized, "-")

	if sanitized == "" {
		sanitized = "unknown"
	}

	// Truncate if too long (max 32 chars for hostname part)
	if len(sanitized) > 32 {
		sanitized = sanitized[:32]
	}

	return fmt.Sprintf("%s-%s", sanitized, uuid.New().String()[:8])
}

// Start begins processing jobs
func (w *Worker) Start(ctx context.Context) error {
	defer close(w.stopped) // Signal that worker has fully stopped

	w.logger.Info("worker started",
		"worker_id", w.workerID,
		"max_concurrent", w.maxConcurrent,
		"poll_interval", w.pollInterval,
		"heartbeat_interval", w.heartbeatInt,
	)

	// Start preemption monitoring if configured
	if w.preemptor != nil {
		w.logger.Info("starting preemption monitoring", "preemptor", w.preemptor.Name())
		preemptEvents, err := w.preemptor.Watch(ctx)
		if err != nil {
			w.logger.Error("failed to start preemptor", "error", err)
		} else {
			w.logger.Info("preemption monitoring active", "preemptor", w.preemptor.Name())
			go w.watchPreemption(preemptEvents)
		}
	} else {
		w.logger.Info("preemption monitoring disabled")
	}

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.tryClaimJob(ctx)
		case <-w.preempted:
			w.logger.Warn("preemption detected, stopping job claims", "worker_id", w.workerID)
			// Preemption handling is done in watchPreemption
			// Just wait for jobs to complete or be released
			w.waitForJobsToComplete()
			w.logger.Info("all jobs completed after preemption", "worker_id", w.workerID)

			// Trigger application shutdown if shutdowner is set
			if w.shutdowner != nil {
				w.logger.Info("triggering application shutdown after preemption")
				if err := w.shutdowner(); err != nil {
					w.logger.Error("failed to trigger shutdown", "error", err)
				}
			}
			return nil
		case <-w.done:
			w.logger.Info("worker stopping gracefully", "worker_id", w.workerID)
			// Set shutdown flag to stop accepting new jobs
			w.jobs.shutdownMu.Lock()
			w.jobs.shuttingDown = true
			w.jobs.shutdownMu.Unlock()
			// Wait for existing jobs to finish
			w.waitForJobsToComplete()
			w.logger.Info("worker stopped gracefully", "worker_id", w.workerID)
			return nil
		case <-ctx.Done():
			w.logger.Info("worker context cancelled", "worker_id", w.workerID)
			w.jobs.shutdownMu.Lock()
			w.jobs.shuttingDown = true
			w.jobs.shutdownMu.Unlock()
			w.waitForJobsToComplete()
			return ctx.Err()
		}
	}
}

// watchPreemption monitors for preemption events
func (w *Worker) watchPreemption(events <-chan preemption.Event) {
	for event := range events {
		w.logger.Warn("preemption event received",
			"worker_id", w.workerID,
			"event_type", event.EventType,
			"not_before", event.NotBefore,
			"description", event.Description,
		)

		// Stop accepting new jobs immediately
		w.jobs.shutdownMu.Lock()
		w.jobs.shuttingDown = true
		w.jobs.shutdownMu.Unlock()

		w.logger.Warn("preemption detected, immediately releasing all active jobs", "worker_id", w.workerID)

		// Immediately release all active jobs back to the queue
		ctx := context.Background()
		w.jobs.cancelAllActiveJobs(ctx, w.pool, w.workerID, w.logger)

		// Signal that we've been preempted
		close(w.preempted)
		return
	}
}

// waitForJobsToComplete waits for all active jobs to complete
func (w *Worker) waitForJobsToComplete() {
	// Fill the semaphore to prevent new jobs
	for i := 0; i < w.maxConcurrent; i++ {
		w.jobs.inProgress <- struct{}{}
	}

	// Check if there are any active jobs
	w.jobs.activeMu.RLock()
	activeCount := len(w.jobs.active)
	w.jobs.activeMu.RUnlock()

	if activeCount == 0 {
		w.logger.Info("no active jobs to wait for", "worker_id", w.workerID)
		return
	}

	w.logger.Info("waiting for active jobs to complete", "worker_id", w.workerID, "active_jobs", activeCount)

	// Poll until all jobs are done (check every 100ms)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(5 * time.Minute) // Maximum wait time
	for {
		select {
		case <-ticker.C:
			w.jobs.activeMu.RLock()
			remaining := len(w.jobs.active)
			w.jobs.activeMu.RUnlock()

			if remaining == 0 {
				w.logger.Info("all active jobs completed", "worker_id", w.workerID)
				return
			}
		case <-timeout:
			w.jobs.activeMu.RLock()
			remaining := len(w.jobs.active)
			w.jobs.activeMu.RUnlock()
			w.logger.Warn("timeout waiting for jobs to complete", "worker_id", w.workerID, "remaining_jobs", remaining)
			return
		}
	}
}

// Stop gracefully stops the worker
func (w *Worker) Stop() {
	close(w.done)
}

// Stopped returns a channel that is closed when the worker has fully stopped
func (w *Worker) Stopped() <-chan struct{} {
	return w.stopped
}

// tryClaimJob attempts to claim and process a job
func (w *Worker) tryClaimJob(ctx context.Context) {
	// Check if we're shutting down
	w.jobs.shutdownMu.RLock()
	if w.jobs.shuttingDown {
		w.jobs.shutdownMu.RUnlock()
		w.logger.Debug("skipping job claim - worker is shutting down")
		return
	}
	w.jobs.shutdownMu.RUnlock()

	// Check if we have capacity for more jobs
	select {
	case w.jobs.inProgress <- struct{}{}:
		// Get current active job count
		w.jobs.activeMu.RLock()
		activeCount := len(w.jobs.active)
		w.jobs.activeMu.RUnlock()

		w.logger.Debug("attempting to claim job",
			"worker_id", w.workerID,
			"active_jobs", activeCount,
			"max_concurrent", w.maxConcurrent,
		)

		// Capacity available, try to claim a job
		build, err := db.ClaimNextBuild(ctx, w.pool, w.workerID)
		if err != nil {
			w.logger.Error("failed to claim build", "error", err)
			<-w.jobs.inProgress // Release semaphore
			return
		}

		if build == nil {
			// No jobs available
			w.logger.Debug("no jobs available in queue")
			<-w.jobs.inProgress // Release semaphore
			return
		}

		w.logger.Info("claimed job",
			"build_key", build.BuildKey,
			"worker_id", w.workerID,
			"active_jobs", activeCount+1,
		)

		// Create a cancellable context for this job
		jobCtx, cancel := context.WithCancel(context.Background())
		w.jobs.addActiveJob(build.BuildKey, cancel)

		// Process the job in a goroutine
		go func() {
			defer func() { <-w.jobs.inProgress }() // Release semaphore when done
			defer w.jobs.removeActiveJob(build.BuildKey)
			defer cancel()

			w.processBuild(jobCtx, build)
		}()
	default:
		// No capacity, skip this cycle
		w.logger.Debug("at max capacity, skipping claim attempt", "max_concurrent", w.maxConcurrent)
	}
}

// processBuild processes a single build job
func (w *Worker) processBuild(ctx context.Context, build *db.Build) {
	startTime := time.Now()
	w.logger.Info("processing build",
		"build_key", build.BuildKey,
		"worker_id", w.workerID,
		"attempt", build.Attempt,
		"version", build.CaddyVersion,
		"os", build.OS,
		"arch", build.Arch,
		"plugins", len(build.Plugins),
		"replacements", len(build.Replacements),
	)

	// Start heartbeat goroutine
	defer w.startHeartbeat(ctx, build.BuildKey)()

	// Prepare storage key
	// Use first byte (2 hex chars) of build key as prefix to avoid too many files in one directory
	// Store by hash only - no need for version/os/arch in path since hash uniquely identifies the build
	prefix := build.BuildKey[:2]
	storageKey := fmt.Sprintf("builds/%s/%s", prefix, build.BuildKey)

	w.logger.Info("starting build execution",
		"build_key", build.BuildKey,
		"storage_key", storageKey,
	)

	// Build the binary
	buildReq := builder.BuildRequest{
		BuildKey:     build.BuildKey,
		CaddyVersion: build.CaddyVersion,
		Plugins:      build.Plugins,
		Replacements: build.Replacements,
		OS:           build.OS,
		Arch:         build.Arch,
		Attempt:      build.Attempt,
		StorageKey:   storageKey,
	}

	result, err := w.builder.Build(ctx, buildReq)
	duration := time.Since(startTime)

	if err != nil {
		// Check if the error was due to context cancellation (preemption)
		if ctx.Err() == context.Canceled {
			w.logger.Warn("build cancelled due to preemption",
				"build_key", build.BuildKey,
				"worker_id", w.workerID,
				"duration", duration,
			)
			// Job will be rescued by janitor when heartbeat expires
			return
		}

		errMsg := fmt.Sprintf("build failed: %v", err)

		// Check if this will be retried or is terminal failure
		if build.Attempt < build.MaxAttempts {
			w.logger.Warn("build failed, will retry automatically",
				"build_key", build.BuildKey,
				"worker_id", w.workerID,
				"error", err,
				"duration", duration,
				"attempt", build.Attempt,
				"max_attempts", build.MaxAttempts,
				"next_attempt", build.Attempt+1,
			)
		} else {
			w.logger.Error("build failed terminally after max attempts",
				"build_key", build.BuildKey,
				"worker_id", w.workerID,
				"error", err,
				"duration", duration,
				"attempt", build.Attempt,
				"max_attempts", build.MaxAttempts,
			)
		}

		// Update status to failed (will auto-retry if attempts remain)
		updateErr := db.UpdateBuildStatus(ctx, w.pool, build.BuildKey, db.BuildStatusFailed, &errMsg, nil, false)
		if updateErr != nil {
			w.logger.Error("failed to update build status",
				"build_key", build.BuildKey,
				"error", updateErr,
			)
		}
		return
	}

	// Update status to completed
	w.logger.Info("build completed successfully",
		"build_key", build.BuildKey,
		"worker_id", w.workerID,
		"storage_key", result.StorageKey,
		"has_signature", result.HasSignature,
		"duration", duration,
	)

	err = db.UpdateBuildStatus(ctx, w.pool, build.BuildKey, db.BuildStatusCompleted, nil, &result.StorageKey, result.HasSignature)
	if err != nil {
		w.logger.Error("failed to update build status to completed",
			"build_key", build.BuildKey,
			"worker_id", w.workerID,
			"error", err,
		)
	}
}

// startHeartbeat starts a heartbeat goroutine for a build and returns a cancel function
// The cancel function should be deferred to ensure the heartbeat stops when the build completes
func (w *Worker) startHeartbeat(ctx context.Context, buildKey string) context.CancelFunc {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	go w.heartbeat(heartbeatCtx, buildKey)
	return cancel
}

// heartbeat periodically updates the heartbeat timestamp for a build
func (w *Worker) heartbeat(ctx context.Context, buildKey string) {
	ticker := time.NewTicker(w.heartbeatInt)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := db.UpdateHeartbeat(ctx, w.pool, buildKey, w.workerID)
			if err != nil {
				w.logger.Error("failed to update heartbeat",
					"build_key", buildKey,
					"worker_id", w.workerID,
					"error", err,
				)
				return
			}
			w.logger.Debug("heartbeat updated",
				"build_key", buildKey,
				"worker_id", w.workerID,
			)
		case <-ctx.Done():
			return
		}
	}
}
