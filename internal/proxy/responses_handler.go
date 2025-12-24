package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/background"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/responses"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/eternisai/enchanted-proxy/internal/title_generation"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleResponsesAPI handles requests to OpenAI's Responses API (GPT-5 Pro, GPT-4.5+).
//
// This handler uses OpenAI's background mode + polling approach to avoid timeout issues.
//
// Flow:
//  1. Fetch previous response_id from Firestore (for continuation)
//  2. Transform request: add store=true, background=true, reasoning.effort=high
//  3. Make HTTP request to OpenAI /responses endpoint with background=true
//  4. Get response_id immediately (response status = "queued")
//  5. Save initial message with generationState="thinking" to Firestore
//  6. Start background polling worker to monitor OpenAI status
//  7. Return immediately to client (202 Accepted)
//  8. Worker polls OpenAI and updates Firestore as status changes
//  9. Client listens to Firestore real-time updates for progress
//
// Parameters:
//   - c: Gin context
//   - requestBody: Original request body from client
//   - provider: Provider config (contains BaseURL, APIKey)
//   - model: Model ID (e.g., "gpt-5-pro")
//   - log: Logger
//   - trackingService: Request tracking service
//   - messageService: Message storage service
//   - titleService: Title generation service
//   - pollingManager: Background polling manager
//   - cfg: Application configuration
//
// Returns:
//   - error: If handling failed
func handleResponsesAPI(
	c *gin.Context,
	requestBody []byte,
	provider *routing.ProviderConfig,
	model string,
	log *logger.Logger,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	titleService *title_generation.Service,
	pollingManager *background.PollingManager,
	cfg *config.Config,
) error {
	// Validate required parameters
	if provider == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Provider configuration is nil"})
		return fmt.Errorf("provider is nil")
	}
	if pollingManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Polling manager not initialized"})
		return fmt.Errorf("pollingManager is nil")
	}
	if log == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Logger not initialized"})
		return fmt.Errorf("logger is nil")
	}

	// Extract chat ID and message ID from headers
	chatID := c.GetHeader("X-Chat-ID")
	messageID := c.GetHeader("X-Message-ID")

	// If headers are missing, try to extract from request body as fallback
	if chatID == "" || messageID == "" {
		var reqBody map[string]interface{}
		if err := json.Unmarshal(requestBody, &reqBody); err == nil {
			if chatID == "" {
				if bodyID, ok := reqBody["chatId"].(string); ok && bodyID != "" {
					chatID = bodyID
					log.Info("using chatId from request body (header missing)")
				}
			}
			if messageID == "" {
				if bodyID, ok := reqBody["messageId"].(string); ok && bodyID != "" {
					messageID = bodyID
					log.Info("using messageId from request body (header missing)")
				}
			}
		}
	}

	// If still missing after checking body, generate fallback values
	if chatID == "" {
		chatID = uuid.New().String()
		log.Warn("X-Chat-ID header and body field missing, generated fallback",
			slog.String("generated_chat_id", chatID))
	}
	if messageID == "" {
		messageID = uuid.New().String()
		log.Warn("X-Message-ID header and body field missing, generated fallback",
			slog.String("generated_message_id", messageID))
	}

	log.Info("GPT-5 Pro request headers",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID))

	// Get user ID for response_id retrieval
	userID, exists := auth.GetUserID(c)
	if !exists {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User ID not found in context"})
		return fmt.Errorf("user ID not found in context")
	}

	// Step 1: Fetch previous response_id from Firestore (for conversation continuation)
	var previousResponseID string
	if messageService != nil {
		prevID, err := messageService.GetResponseID(c.Request.Context(), userID, chatID)
		if err != nil {
			// Log error but continue (conversation will start fresh)
			log.Error("failed to fetch previous response_id", slog.String("error", err.Error()))
		} else if prevID != "" {
			previousResponseID = prevID
		}
	}

	// Step 2: Check if this is the first user message and trigger title generation
	if titleService != nil && len(requestBody) > 0 {
		if isFirst, firstMessage := isFirstUserMessage(requestBody); isFirst {
			// Get encryption flag from context
			var encryptionEnabled *bool
			if val, exists := c.Get("encryptionEnabled"); exists {
				if boolPtr, ok := val.(*bool); ok {
					encryptionEnabled = boolPtr
				}
			}

			// Get platform for title generation
			platform := c.GetHeader("X-Client-Platform")
			if platform == "" {
				platform = "mobile"
			}

			// Queue async title generation (non-blocking)
			// Uses GLM-4.6 for first 2 attempts, Llama 3.3 70B for final fallback
			go titleService.QueueTitleGeneration(context.Background(), title_generation.TitleGenerationRequest{
				UserID:            userID,
				ChatID:            chatID,
				FirstMessage:      firstMessage,
				Model:             model, // Ignored - hardcoded models used instead
				BaseURL:           provider.BaseURL, // Ignored - looked up from routing
				Platform:          platform,
				EncryptionEnabled: encryptionEnabled,
			})
		}
	}

	// Extract encryption setting (used for placeholder save and polling job)
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// Save placeholder message immediately (before making request)
	if messageService != nil {
		// Save placeholder synchronously (fast operation)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = messageService.SaveThinkingMessage(ctx, userID, chatID, messageID, model, encryptionEnabled)
		cancel()
	}

	// Step 3: Transform request for Responses API (adds background=true, reasoning.effort=high)
	adapter := responses.NewAdapter()
	transformedBody, err := adapter.TransformRequest(requestBody, previousResponseID)
	if err != nil {
		log.Error("failed to transform request",
			slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to transform request for Responses API"})
		return fmt.Errorf("failed to transform request: %w", err)
	}

	// Log the transformed request body (for debugging)
	var requestDebug map[string]interface{}
	if err := json.Unmarshal(transformedBody, &requestDebug); err == nil {
		log.Info("transformed request for OpenAI",
			slog.String("model", model),
			slog.Bool("background", requestDebug["background"] == true),
			slog.Bool("store", requestDebug["store"] == true),
			slog.String("previous_response_id", fmt.Sprintf("%v", requestDebug["previous_response_id"])),
			slog.String("reasoning_effort", func() string {
				if r, ok := requestDebug["reasoning"].(map[string]interface{}); ok {
					return fmt.Sprintf("%v", r["effort"])
				}
				return "none"
			}()))
	}

	// Step 4: Make HTTP request to OpenAI /responses endpoint with background=true
	// Note: provider.BaseURL already includes "/v1", so we just append "/responses"
	targetURL := provider.BaseURL + "/responses"

	log.Info("submitting request to OpenAI Responses API",
		slog.String("url", targetURL),
		slog.String("provider_base_url", provider.BaseURL),
		slog.String("provider_name", provider.Name),
		slog.String("api_type", string(provider.APIType)),
		slog.String("model", model),
		slog.Int("body_size", len(transformedBody)),
		slog.Int("api_key_length", len(provider.APIKey)))

	req, err := http.NewRequestWithContext(c.Request.Context(), "POST", targetURL, bytes.NewReader(transformedBody))
	if err != nil {
		log.Error("failed to create request",
			slog.String("error", err.Error()),
			slog.String("target_url", targetURL))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create upstream request"})
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	req.Header.Set("Content-Type", "application/json")

	// Make request with short timeout (we're just submitting the request, not waiting for completion)
	client := &http.Client{
		Timeout: 30 * time.Second, // Short timeout for initial submission
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Error("failed to submit request to Responses API",
			slog.String("error", err.Error()),
			slog.String("target_url", targetURL))
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to Responses API"})
		return fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	// Check for errors
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)

		// Try to parse error as JSON for better logging
		var errorResponse map[string]interface{}
		errorMessage := string(body)
		if json.Unmarshal(body, &errorResponse) == nil {
			if errObj, ok := errorResponse["error"].(map[string]interface{}); ok {
				log.Error("OpenAI Responses API error",
					slog.Int("status_code", resp.StatusCode),
					slog.String("url", targetURL),
					slog.String("model", model),
					slog.String("error_type", fmt.Sprintf("%v", errObj["type"])),
					slog.String("error_message", fmt.Sprintf("%v", errObj["message"])),
					slog.String("error_code", fmt.Sprintf("%v", errObj["code"])),
					slog.String("full_response", string(body)))
			} else {
				log.Error("OpenAI Responses API error (non-standard format)",
					slog.Int("status_code", resp.StatusCode),
					slog.String("url", targetURL),
					slog.String("model", model),
					slog.String("response_body", string(body)))
			}
		} else {
			log.Error("OpenAI Responses API error (non-JSON response)",
				slog.Int("status_code", resp.StatusCode),
				slog.String("url", targetURL),
				slog.String("model", model),
				slog.String("response_body", errorMessage))
		}

		c.Data(resp.StatusCode, "application/json", body)
		return fmt.Errorf("Responses API error: %d", resp.StatusCode)
	}

	// Step 5: Parse response to get response_id
	// OpenAI returns: {"id": "resp_abc123", "status": "queued", ...}
	var bgResponse background.ResponseStatus
	if err := json.NewDecoder(resp.Body).Decode(&bgResponse); err != nil {
		log.Error("failed to decode background response",
			slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to parse OpenAI response"})
		return fmt.Errorf("failed to decode response: %w", err)
	}

	log.Info("received background response from OpenAI",
		slog.String("response_id", bgResponse.ID),
		slog.String("status", bgResponse.Status),
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID))

	// Step 6: Save response_id to Firestore for conversation continuation
	if err := messageService.SaveResponseID(c.Request.Context(), userID, chatID, bgResponse.ID); err != nil {
		log.Error("failed to save response_id",
			slog.String("response_id", bgResponse.ID),
			slog.String("error", err.Error()))
		// Continue anyway - this is not critical for polling
	}

	// Step 7: Start background polling worker
	// This worker will poll OpenAI every few seconds and update Firestore as status changes
	pollingJob := background.PollingJob{
		ResponseID:        bgResponse.ID,
		UserID:            userID,
		ChatID:            chatID,
		MessageID:         messageID,
		Model:             model,
		EncryptionEnabled: encryptionEnabled,
		StartedAt:         time.Now(),
	}

	// CRITICAL: Use context.Background() instead of c.Request.Context()
	// The polling worker MUST continue even if the client disconnects
	// Otherwise long-running GPT-5 Pro requests will be killed when client app closes
	if err := pollingManager.StartPolling(context.Background(), pollingJob, provider.APIKey, provider.BaseURL, provider.TokenMultiplier); err != nil {
		log.Error("failed to start polling worker",
			slog.String("response_id", bgResponse.ID),
			slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to start background polling"})
		return fmt.Errorf("failed to start polling: %w", err)
	}

	log.Info("started background polling worker",
		slog.String("response_id", bgResponse.ID),
		slog.String("message_id", messageID),
		slog.Int("active_workers", pollingManager.GetActiveCount()))

	// Step 8: Return immediately to client
	// Client will listen to Firestore for real-time updates
	c.JSON(http.StatusAccepted, gin.H{
		"message_id":  messageID,
		"response_id": bgResponse.ID,
		"status":      "queued",
		"message":     "Request submitted successfully. Listen to Firestore for updates.",
	})

	// Log request to database (with multiplier for cost tracking)
	logRequestToDatabaseWithProvider(c, trackingService, model, nil, provider.Name, provider.TokenMultiplier)

	return nil
}

// streamToClientWithResponseID streams chunks to client and extracts response_id.
// This is similar to streamToClient but also extracts the response_id from the first chunk.
func streamToClientWithResponseID(
	c *gin.Context,
	subscriber *streaming.StreamSubscriber,
	session *streaming.StreamSession,
	log *logger.Logger,
	adapter *responses.Adapter,
) {
	// CRITICAL: Always unsubscribe when done to prevent resource leak
	defer func() {
		session.Unsubscribe(subscriber.ID)
		log.Debug("client stream finished (Responses API)",
			slog.String("subscriber_id", subscriber.ID))
	}()

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Get the response writer flusher
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		log.Error("response writer doesn't support flushing")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Streaming not supported"})
		return
	}

	// Stream chunks to client
	for {
		select {
		case chunk, ok := <-subscriber.Ch:
			if !ok {
				// Channel closed, streaming complete
				log.Debug("subscriber channel closed",
					slog.String("subscriber_id", subscriber.ID))
				return
			}

			// Extract response_id from first chunk (if not already extracted)
			if session.GetResponseID() == "" {
				responseID := adapter.ExtractResponseID(chunk.Line)
				if responseID != "" {
					session.SetResponseID(responseID)
				}
			}

			// Write chunk to client
			if _, err := c.Writer.WriteString(chunk.Line + "\n"); err != nil {
				log.Error("failed to write chunk to client",
					slog.String("error", err.Error()),
					slog.String("subscriber_id", subscriber.ID))
				return
			}

			// Flush immediately (SSE requirement)
			flusher.Flush()

			// If this is the final chunk, we're done
			if chunk.IsFinal {
				log.Debug("final chunk sent to client",
					slog.String("subscriber_id", subscriber.ID))
				return
			}

		case <-c.Request.Context().Done():
			// Client disconnected
			log.Debug("client disconnected",
				slog.String("subscriber_id", subscriber.ID))
			return

		case <-subscriber.Context().Done():
			// Subscriber cancelled
			log.Debug("subscriber cancelled",
				slog.String("subscriber_id", subscriber.ID))
			return
		}
	}
}
