package background

import (
	"context"
	"fmt"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"log/slog"
)

// PollingWorker polls OpenAI for a single background response.
//
// Lifecycle:
//  1. Start polling every N seconds
//  2. Update Firestore generationState as status changes
//  3. When completed: fetch full response, save to Firestore
//  4. When failed: save error to Firestore
//  5. Cleanup and exit
//
// Thread-safety: Each worker runs in its own goroutine.
type PollingWorker struct {
	job            PollingJob
	openAIClient   *OpenAIClient
	messageService *messaging.Service
	logger         *logger.Logger
	pollCount      int
	cfg            *config.Config
}

// NewPollingWorker creates a new polling worker.
func NewPollingWorker(
	job PollingJob,
	openAIClient *OpenAIClient,
	messageService *messaging.Service,
	logger *logger.Logger,
	cfg *config.Config,
) *PollingWorker {
	return &PollingWorker{
		job:            job,
		openAIClient:   openAIClient,
		messageService: messageService,
		logger:         logger.WithComponent("polling_worker"),
		cfg:            cfg,
	}
}

// Run starts the polling loop.
//
// This method blocks until:
//   - Response is completed
//   - Response failed
//   - Timeout reached
//   - Context cancelled
//
// Returns:
//   - error: If polling failed
func (w *PollingWorker) Run(ctx context.Context) error {
	w.logger.Info("starting background polling",
		slog.String("response_id", w.job.ResponseID))

	// Create timeout context (default: 30 minutes)
	timeoutDuration := time.Duration(w.cfg.BackgroundPollingTimeout) * time.Minute
	ctx, cancel := context.WithTimeout(ctx, timeoutDuration)
	defer cancel()

	// Initial polling interval (start fast, slow down later)
	pollInterval := time.Duration(w.cfg.BackgroundPollingInterval) * time.Second
	maxPollInterval := time.Duration(w.cfg.BackgroundPollingMaxInterval) * time.Second

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Timeout or cancellation
			w.logger.Warn("polling cancelled or timed out",
				slog.String("response_id", w.job.ResponseID),
				slog.Int("poll_count", w.pollCount),
				slog.Duration("elapsed", time.Since(w.job.StartedAt)))

			// Mark as failed due to timeout
			if err := w.saveFailure("Polling timeout after 30 minutes"); err != nil {
				w.logger.Error("failed to save timeout state",
					slog.String("response_id", w.job.ResponseID),
					slog.String("error", err.Error()))
			}

			return ctx.Err()

		case <-ticker.C:
			w.pollCount++

			// Poll OpenAI
			status, err := w.openAIClient.GetResponseStatus(ctx, w.job.ResponseID)
			if err != nil {
				w.logger.Error("failed to poll OpenAI",
					slog.String("response_id", w.job.ResponseID),
					slog.String("error", err.Error()),
					slog.Int("poll_count", w.pollCount))

				// Don't fail immediately - retry on next tick
				// OpenAI might have transient issues
				continue
			}

			// Update Firestore with current status
			generationState := MapStatusToGenerationState(status.Status)
			if err := w.updateFirestoreState(ctx, generationState); err != nil {
				w.logger.Error("failed to update Firestore state",
					slog.String("response_id", w.job.ResponseID),
					slog.String("state", generationState),
					slog.String("error", err.Error()))
				// Continue polling even if Firestore update fails
			}

			// Handle terminal states
			switch status.Status {
			case "completed":
				w.logger.Info("response completed",
					slog.String("response_id", w.job.ResponseID),
					slog.Int("poll_count", w.pollCount),
					slog.Duration("duration", time.Since(w.job.StartedAt)))

				// Fetch and save full response
				if err := w.fetchAndSaveResponse(ctx); err != nil {
					w.logger.Error("failed to save completed response",
						slog.String("response_id", w.job.ResponseID),
						slog.String("error", err.Error()))
					return err
				}

				return nil // Done

			case "failed":
				w.logger.Error("response failed",
					slog.String("response_id", w.job.ResponseID),
					slog.Int("poll_count", w.pollCount),
					slog.Duration("duration", time.Since(w.job.StartedAt)))

				// Save error state
				errorMsg := "Response failed"
				if status.Error != nil {
					errorMsg = status.Error.Message
				}
				if err := w.saveFailure(errorMsg); err != nil {
					w.logger.Error("failed to save error state",
						slog.String("response_id", w.job.ResponseID),
						slog.String("error", err.Error()))
				}

				return fmt.Errorf("response failed: %s", errorMsg)

			case "in_progress", "queued":
				// Still processing - continue polling
				w.logger.Debug("response still processing",
					slog.String("response_id", w.job.ResponseID),
					slog.String("status", status.Status),
					slog.Int("poll_count", w.pollCount),
					slog.Duration("elapsed", time.Since(w.job.StartedAt)))

				// Slow down polling after initial phase (after 10 polls = ~20 seconds)
				if w.pollCount > 10 && pollInterval < maxPollInterval {
					pollInterval = maxPollInterval
					ticker.Reset(pollInterval)
					w.logger.Debug("slowed down polling interval",
						slog.String("response_id", w.job.ResponseID),
						slog.Duration("interval", pollInterval))
				}

			default:
				w.logger.Warn("unknown status from OpenAI",
					slog.String("response_id", w.job.ResponseID),
					slog.String("status", status.Status))
			}
		}
	}
}

// updateFirestoreState updates the generationState in Firestore.
func (w *PollingWorker) updateFirestoreState(ctx context.Context, state string) error {
	// Use synchronous update to ensure state is saved immediately
	return w.messageService.UpdateGenerationStateSync(
		ctx,
		w.job.UserID,
		w.job.ChatID,
		w.job.MessageID,
		state,
		"", // No error message
	)
}

// fetchAndSaveResponse fetches the completed response from OpenAI and saves to Firestore.
func (w *PollingWorker) fetchAndSaveResponse(ctx context.Context) error {
	// Fetch full response content
	content, err := w.openAIClient.GetResponseContent(ctx, w.job.ResponseID)
	if err != nil {
		return fmt.Errorf("failed to fetch response content: %w", err)
	}

	// Extract text content
	textContent := ExtractContent(content)
	if textContent == "" {
		w.logger.Warn("no content in completed response",
			slog.String("response_id", w.job.ResponseID))
	}

	w.logger.Debug("fetched completed response",
		slog.String("response_id", w.job.ResponseID),
		slog.Int("content_length", len(textContent)))

	// Save to Firestore using StoreMessageAsync
	now := time.Now()
	msg := messaging.MessageToStore{
		UserID:                w.job.UserID,
		ChatID:                w.job.ChatID,
		MessageID:             w.job.MessageID,
		IsFromUser:            false,
		Content:               textContent,
		IsError:               false,
		EncryptionEnabled:     w.job.EncryptionEnabled,
		Model:                 w.job.Model,
		GenerationState:       "completed",
		GenerationCompletedAt: &now,
	}

	// Use background context to ensure save completes even if request context is cancelled
	saveCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := w.messageService.StoreMessageAsync(saveCtx, msg); err != nil {
		return fmt.Errorf("failed to save completed message: %w", err)
	}

	w.logger.Debug("saved completed response to Firestore",
		slog.String("response_id", w.job.ResponseID))

	return nil
}

// saveFailure saves a failed state to Firestore.
func (w *PollingWorker) saveFailure(errorMsg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return w.messageService.UpdateGenerationStateSync(
		ctx,
		w.job.UserID,
		w.job.ChatID,
		w.job.MessageID,
		"failed",
		errorMsg,
	)
}
