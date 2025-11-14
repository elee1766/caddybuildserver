//go:build linux

package sigusr1

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/preemption"
)

// SIGUSR1Preemptor listens for SIGUSR1 signals and triggers preemption
type SIGUSR1Preemptor struct {
	logger *slog.Logger
}

// NewSIGUSR1Preemptor creates a new SIGUSR1 preemptor
func NewSIGUSR1Preemptor(logger *slog.Logger) *SIGUSR1Preemptor {
	return &SIGUSR1Preemptor{
		logger: logger,
	}
}

// Name returns the name of this preemptor
func (s *SIGUSR1Preemptor) Name() string {
	return "sigusr1"
}

// Watch listens for SIGUSR1 signals and sends preemption events
func (s *SIGUSR1Preemptor) Watch(ctx context.Context) (<-chan preemption.Event, error) {
	events := make(chan preemption.Event, 1)

	// Create signal channel
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGUSR1)

	go func() {
		defer close(events)
		defer signal.Stop(sigChan)

		s.logger.Info("sigusr1 preemptor started", "signal", "SIGUSR1")

		select {
		case <-ctx.Done():
			s.logger.Info("sigusr1 preemptor stopped")
			return
		case sig := <-sigChan:
			s.logger.Warn("preemption signal received",
				"signal", sig.String(),
				"pid", os.Getpid(),
			)

			// Send preemption event
			events <- preemption.Event{
				EventType:   "Preempt",
				NotBefore:   time.Now(),
				Description: "SIGUSR1 signal received",
			}

			s.logger.Info("preemption event sent, shutting down preemptor")
			return
		}
	}()

	return events, nil
}
