package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/preemption"
)

const (
	// Azure Scheduled Events metadata endpoint
	azureMetadataEndpoint = "http://169.254.169.254/metadata/scheduledevents?api-version=2020-07-01"
)

// AzureScheduledEvent represents an Azure Scheduled Event
type AzureScheduledEvent struct {
	EventID      string    `json:"EventId"`
	EventType    string    `json:"EventType"`
	ResourceType string    `json:"ResourceType"`
	Resources    []string  `json:"Resources"`
	EventStatus  string    `json:"EventStatus"`
	NotBefore    time.Time `json:"NotBefore"`
	Description  string    `json:"Description"`
	EventSource  string    `json:"EventSource"`
}

// AzureScheduledEventsResponse represents the response from Azure metadata endpoint
type AzureScheduledEventsResponse struct {
	DocumentIncarnation int                   `json:"DocumentIncarnation"`
	Events              []AzureScheduledEvent `json:"Events"`
}

// AzurePreemptor monitors Azure Scheduled Events for preemption signals
type AzurePreemptor struct {
	pollInterval time.Duration
	client       *http.Client
	logger       *slog.Logger
}

// NewAzurePreemptor creates a new Azure preemptor
func NewAzurePreemptor(pollInterval time.Duration, logger *slog.Logger) *AzurePreemptor {
	return &AzurePreemptor{
		pollInterval: pollInterval,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		logger: logger,
	}
}

// Name returns the name of this preemptor
func (a *AzurePreemptor) Name() string {
	return "azure"
}

// Watch polls Azure Scheduled Events and sends preemption events to the channel
func (a *AzurePreemptor) Watch(ctx context.Context) (<-chan preemption.Event, error) {
	events := make(chan preemption.Event, 1)

	go func() {
		defer close(events)

		a.logger.Info("azure preemptor started",
			"poll_interval", a.pollInterval,
			"endpoint", azureMetadataEndpoint,
		)

		ticker := time.NewTicker(a.pollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				a.logger.Info("azure preemptor stopped")
				return
			case <-ticker.C:
				a.checkScheduledEvents(ctx, events)
			}
		}
	}()

	return events, nil
}

// checkScheduledEvents queries the Azure metadata endpoint for scheduled events
func (a *AzurePreemptor) checkScheduledEvents(ctx context.Context, events chan<- preemption.Event) {
	req, err := http.NewRequestWithContext(ctx, "GET", azureMetadataEndpoint, nil)
	if err != nil {
		a.logger.Error("failed to create request", "error", err)
		return
	}

	// Azure requires this header
	req.Header.Set("Metadata", "true")

	resp, err := a.client.Do(req)
	if err != nil {
		// Don't log errors if not on Azure (connection refused is expected)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		a.logger.Warn("unexpected status from Azure metadata",
			"status", resp.StatusCode,
			"body", string(body),
		)
		return
	}

	var scheduledEvents AzureScheduledEventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&scheduledEvents); err != nil {
		a.logger.Error("failed to decode scheduled events", "error", err)
		return
	}

	// Check for preemption events
	for _, event := range scheduledEvents.Events {
		if event.EventType == "Preempt" || event.EventType == "Terminate" {
			a.logger.Warn("preemption detected",
				"event_id", event.EventID,
				"event_type", event.EventType,
				"not_before", event.NotBefore,
				"resources", event.Resources,
				"description", event.Description,
			)

			// Send the event (non-blocking)
			select {
			case events <- preemption.Event{
				EventType:   event.EventType,
				NotBefore:   event.NotBefore,
				Description: fmt.Sprintf("%s: %s", event.EventSource, event.Description),
			}:
			case <-ctx.Done():
				return
			default:
				// Channel full, event already pending
			}

			// Acknowledge the event to Azure
			a.acknowledgeEvent(ctx, event.EventID)
		}
	}
}

// acknowledgeEvent sends an acknowledgment to Azure for a scheduled event
func (a *AzurePreemptor) acknowledgeEvent(ctx context.Context, eventID string) {
	ackPayload := map[string]any{
		"StartRequests": []map[string]string{
			{"EventId": eventID},
		},
	}

	bodyBytes, err := json.Marshal(ackPayload)
	if err != nil {
		a.logger.Error("failed to marshal acknowledgment", "error", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", azureMetadataEndpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		a.logger.Error("failed to create ack request", "error", err)
		return
	}

	req.Header.Set("Metadata", "true")
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		a.logger.Error("failed to acknowledge event", "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		a.logger.Warn("failed to acknowledge event",
			"status", resp.StatusCode,
			"body", string(body),
		)
		return
	}

	a.logger.Info("acknowledged scheduled event", "event_id", eventID)
}
