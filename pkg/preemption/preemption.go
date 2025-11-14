package preemption

import (
	"context"
	"time"
)

// Event represents a preemption event
type Event struct {
	// EventType is the type of event (e.g., "Preempt", "Terminate")
	EventType string

	// NotBefore is the earliest time the preemption will occur
	NotBefore time.Time

	// Description provides additional context
	Description string
}

// Preemptor monitors for preemption signals from the cloud provider
type Preemptor interface {
	// Watch returns a channel that receives preemption events
	// The channel is closed when the context is cancelled or an error occurs
	Watch(ctx context.Context) (<-chan Event, error)

	// Name returns the name of this preemptor (e.g., "azure", "aws", "gcp")
	Name() string
}
