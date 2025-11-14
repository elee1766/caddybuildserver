-- Initial schema for Caddy build server

-- Create ENUM for build status
CREATE TYPE build_status AS ENUM ('pending', 'building', 'completed', 'failed');

-- Create build_jobs table (the active queue of what needs to be built)
CREATE TABLE build_jobs (
    build_key TEXT PRIMARY KEY,
    caddy_version TEXT NOT NULL,
    plugins TEXT[] NOT NULL DEFAULT '{}',
    replacements TEXT[] NOT NULL DEFAULT '{}',
    os TEXT NOT NULL,
    arch TEXT NOT NULL,
    status build_status NOT NULL DEFAULT 'pending',
    storage_key TEXT,
    has_signature BOOLEAN NOT NULL DEFAULT FALSE,
    current_worker_id TEXT,
    locked_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    max_attempts INTEGER NOT NULL DEFAULT 3,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ
);

-- Create build_attempts table (historical record of each attempt)
-- Note: No foreign key constraint so attempts are preserved even when jobs are retired
CREATE TABLE build_attempts (
    id TEXT PRIMARY KEY DEFAULT gen_random_uuid()::text,
    build_key TEXT NOT NULL,
    attempt_number INTEGER NOT NULL,
    worker_id TEXT NOT NULL,
    status build_status NOT NULL,
    error_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    UNIQUE (build_key, attempt_number)
);

-- Indexes for build_jobs
CREATE INDEX idx_build_jobs_status_pending ON build_jobs(status) WHERE status = 'pending';
CREATE INDEX idx_build_jobs_status_failed ON build_jobs(status) WHERE status = 'failed';
CREATE INDEX idx_build_jobs_created_at ON build_jobs(created_at DESC);

-- Indexes for build_attempts
CREATE INDEX idx_build_attempts_build_key ON build_attempts(build_key);
CREATE INDEX idx_build_attempts_worker_id ON build_attempts(worker_id);

---- create above / drop below ----

-- Drop indexes
DROP INDEX IF EXISTS idx_build_attempts_worker_id;
DROP INDEX IF EXISTS idx_build_attempts_build_key;
DROP INDEX IF EXISTS idx_build_jobs_created_at;
DROP INDEX IF EXISTS idx_build_jobs_status_failed;
DROP INDEX IF EXISTS idx_build_jobs_status_pending;

-- Drop tables
DROP TABLE IF EXISTS build_attempts;
DROP TABLE IF EXISTS build_jobs;

-- Drop ENUM
DROP TYPE IF EXISTS build_status;
