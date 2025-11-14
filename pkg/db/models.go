package db

import (
	"time"
)

// BuildStatus represents the current state of a build
type BuildStatus string

const (
	BuildStatusPending   BuildStatus = "pending"
	BuildStatusBuilding  BuildStatus = "building"
	BuildStatusCompleted BuildStatus = "completed"
	BuildStatusFailed    BuildStatus = "failed"
)

// BuildJob represents an active build job in the queue (what needs to be built)
type BuildJob struct {
	BuildKey         string      `json:"build_key" db:"build_key"`             // Primary key, unique hash
	CaddyVersion     string      `json:"caddy_version" db:"caddy_version"`     // e.g., "v2.7.6"
	Plugins          []string    `json:"plugins" db:"plugins"`                 // e.g., ["github.com/caddy-dns/cloudflare"]
	Replacements     []string    `json:"replacements" db:"replacements"`       // e.g., ["old=new"]
	OS               string      `json:"os" db:"os"`                           // e.g., "linux"
	Arch             string      `json:"arch" db:"arch"`                       // e.g., "amd64"
	Status           BuildStatus `json:"status" db:"status"`
	StorageKey       *string     `json:"storage_key,omitempty" db:"storage_key"` // Where completed binary is stored
	HasSignature     bool        `json:"has_signature" db:"has_signature"`       // Whether this build has a signature
	CurrentWorkerID  *string     `json:"current_worker_id,omitempty" db:"current_worker_id"`
	LockedAt         *time.Time  `json:"locked_at,omitempty" db:"locked_at"`
	HeartbeatAt      *time.Time  `json:"heartbeat_at,omitempty" db:"heartbeat_at"`
	MaxAttempts      int         `json:"max_attempts" db:"max_attempts"`
	CreatedAt        time.Time   `json:"created_at" db:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at" db:"updated_at"`
	CompletedAt      *time.Time  `json:"completed_at,omitempty" db:"completed_at"`
}

// BuildAttempt represents a historical record of an attempt to build a job
type BuildAttempt struct {
	ID            string      `json:"id" db:"id"`                               // Unique identifier for this attempt
	BuildKey      string      `json:"build_key" db:"build_key"`
	AttemptNumber int         `json:"attempt_number" db:"attempt_number"`
	WorkerID      string      `json:"worker_id" db:"worker_id"`
	Status        BuildStatus `json:"status" db:"status"`
	ErrorMessage  *string     `json:"error_message,omitempty" db:"error_message"`
	StartedAt     time.Time   `json:"started_at" db:"started_at"`
	CompletedAt   *time.Time  `json:"completed_at,omitempty" db:"completed_at"`
}

// Build represents the API view combining target and attempt info (for backward compatibility)
type Build struct {
	BuildKey      string      `json:"build_key"`
	CaddyVersion  string      `json:"caddy_version"`
	Plugins       []string    `json:"plugins"`
	Replacements  []string    `json:"replacements"`
	OS            string      `json:"os"`
	Arch          string      `json:"arch"`
	Status        BuildStatus `json:"status"`
	ErrorMessage  *string     `json:"error_message,omitempty"`
	StorageKey    *string     `json:"storage_key,omitempty"`
	HasSignature  bool        `json:"has_signature"` // Whether this build has a signature
	WorkerID      *string     `json:"worker_id,omitempty"` // Current worker
	LockedAt      *time.Time  `json:"locked_at,omitempty"`
	HeartbeatAt   *time.Time  `json:"heartbeat_at,omitempty"`
	Attempt       int         `json:"attempt"`        // Current attempt number
	MaxAttempts   int         `json:"max_attempts"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	CompletedAt   *time.Time  `json:"completed_at,omitempty"`
}
