package background

import (
	"context"
	"fmt"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"log/slog"
)

// PollingWorker polls OpenAI for a single background response.
//
// Lifecycle:
//  1. Start polling every N seconds
//  2. Update Firestore generationState as status changes
//  3. When completed: fetch full response, save to Firestore, log token usage
//  4. When failed: save error to Firestore
//  5. Cleanup and exit
//
// Thread-safety: Each worker runs in its own goroutine.
type PollingWorker struct {
	job             PollingJob
	openAIClient    *OpenAIClient
	messageService  *messaging.Service
	trackingService *request_tracking.Service
	logger          *logger.Logger
	pollCount       int
	cfg             *config.Config
	tokenMultiplier float64 // Cost multiplier for this model (e.g., 50× for GPT-5 Pro)
}

// NewPollingWorker creates a new polling worker.
func NewPollingWorker(
	job PollingJob,
	openAIClient *OpenAIClient,
	messageService *messaging.Service,
	trackingService *request_tracking.Service,
	logger *logger.Logger,
	cfg *config.Config,
	tokenMultiplier float64,
) *PollingWorker {
	return &PollingWorker{
		job:             job,
		openAIClient:    openAIClient,
		messageService:  messageService,
		trackingService: trackingService,
		logger:          logger.WithComponent("polling_worker"),
		cfg:             cfg,
		tokenMultiplier: tokenMultiplier,
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

					// CRITICAL: Update Firestore to "failed" so message doesn't stay stuck in "thinking"
					if saveErr := w.saveFailure(fmt.Sprintf("Failed to save response: %v", err)); saveErr != nil {
						w.logger.Error("failed to save failure state",
							slog.String("response_id", w.job.ResponseID),
							slog.String("error", saveErr.Error()))
					}

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
				// Log at Info level every 10 polls so we can see progress in Grafana
				if w.pollCount%10 == 0 {
					w.logger.Info("polling progress",
						slog.String("response_id", w.job.ResponseID),
						slog.String("status", status.Status),
						slog.Int("poll_count", w.pollCount),
						slog.Duration("elapsed", time.Since(w.job.StartedAt)))
				} else {
					w.logger.Debug("response still processing",
						slog.String("response_id", w.job.ResponseID),
						slog.String("status", status.Status),
						slog.Int("poll_count", w.pollCount))
				}

				// Slow down polling after initial phase (after 10 polls = ~20 seconds)
				if w.pollCount > 10 && pollInterval < maxPollInterval {
					pollInterval = maxPollInterval
					ticker.Reset(pollInterval)
					w.logger.Info("slowed down polling interval",
						slog.String("response_id", w.job.ResponseID),
						slog.Duration("new_interval", pollInterval),
						slog.Int("poll_count", w.pollCount))
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
	w.logger.Info("fetching completed response from OpenAI",
		slog.String("response_id", w.job.ResponseID))

	// Fetch full response content
	content, err := w.openAIClient.GetResponseContent(ctx, w.job.ResponseID)
	if err != nil {
		w.logger.Error("failed to fetch response content from OpenAI",
			slog.String("response_id", w.job.ResponseID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to fetch response content: %w", err)
	}

	// Extract text content
	w.logger.Info("extracting content from response",
		slog.String("response_id", w.job.ResponseID),
		slog.Int("output_items", len(content.Output)),
		slog.Int("choices", len(content.Choices)),
		slog.String("status", content.Status))

	textContent := ExtractContent(content)
	if textContent == "" {
		// Log the structure to help debug
		w.logger.Error("failed to extract content from completed response - content is empty",
			slog.String("response_id", w.job.ResponseID),
			slog.Int("output_items", len(content.Output)),
			slog.Int("choices", len(content.Choices)),
			slog.String("status", content.Status),
			slog.String("model", content.Model))
		return fmt.Errorf("extracted content is empty (output_items=%d, choices=%d)", len(content.Output), len(content.Choices))
	}

	w.logger.Info("successfully extracted content from response",
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

	w.logger.Info("saving completed response to Firestore",
		slog.String("response_id", w.job.ResponseID),
		slog.Int("content_length", len(textContent)))

	if err := w.messageService.StoreMessageAsync(saveCtx, msg); err != nil {
		w.logger.Error("failed to save completed message to Firestore",
			slog.String("response_id", w.job.ResponseID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to save completed message: %w", err)
	}

	w.logger.Info("successfully saved completed response to Firestore",
		slog.String("response_id", w.job.ResponseID))

	// Log token usage to database for GPT-5 Pro requests
	if content.Usage == nil {
		w.logger.Warn("no token usage data in completed response",
			slog.String("response_id", w.job.ResponseID))
	} else if w.trackingService == nil {
		// CRITICAL: Tracking service unavailable - cannot log GPT-5 Pro tokens
		// This causes revenue loss and rate limiting bypass
		w.logger.Error("tracking service unavailable - cannot log GPT-5 Pro tokens",
			slog.String("response_id", w.job.ResponseID),
			slog.Int("total_tokens", content.Usage.TotalTokens),
			slog.String("model", w.job.Model),
			slog.String("user_id", w.job.UserID))
		// Note: Consider failing the request or alerting in production
	} else {
		// Calculate plan tokens using multiplier (e.g., 50× for GPT-5 Pro)
		planTokens := int(float64(content.Usage.TotalTokens) * w.tokenMultiplier)

		tokenData := &request_tracking.TokenUsageWithMultiplier{
			PromptTokens:     content.Usage.PromptTokens,
			CompletionTokens: content.Usage.CompletionTokens,
			TotalTokens:      content.Usage.TotalTokens,
			Multiplier:       w.tokenMultiplier,
			PlanTokens:       planTokens,
		}

		requestInfo := request_tracking.RequestInfo{
			UserID:           w.job.UserID,
			Endpoint:         "/v1/responses",
			Model:            w.job.Model,
			Provider:         "GPT 5 Pro",
			PromptTokens:     &content.Usage.PromptTokens,
			CompletionTokens: &content.Usage.CompletionTokens,
			TotalTokens:      &content.Usage.TotalTokens,
			PlanTokens:       &planTokens,
			Multiplier:       &w.tokenMultiplier,
		}

		// Use background context to ensure log completes even if request context cancelled
		logCtx, logCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer logCancel()

		if err := w.trackingService.LogRequestWithPlanTokensAsync(logCtx, requestInfo, tokenData); err != nil {
			w.logger.Error("failed to log token usage for GPT-5 Pro response",
				slog.String("response_id", w.job.ResponseID),
				slog.Int("total_tokens", content.Usage.TotalTokens),
				slog.Int("plan_tokens", planTokens),
				slog.Float64("multiplier", w.tokenMultiplier),
				slog.String("error", err.Error()))
		} else {
			w.logger.Info("logged token usage for GPT-5 Pro response",
				slog.String("response_id", w.job.ResponseID),
				slog.Int("prompt_tokens", content.Usage.PromptTokens),
				slog.Int("completion_tokens", content.Usage.CompletionTokens),
				slog.Int("total_tokens", content.Usage.TotalTokens),
				slog.Int("plan_tokens", planTokens),
				slog.Float64("multiplier", w.tokenMultiplier))
		}
	}

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
