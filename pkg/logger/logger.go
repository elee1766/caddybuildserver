package logger

import (
	"log/slog"
	"os"
)

// New creates a new slog logger with JSON output
func New() *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	return slog.New(handler)
}

// With creates a scoped logger with additional attributes
func With(logger *slog.Logger, component string) *slog.Logger {
	return logger.With(slog.String("component", component))
}
