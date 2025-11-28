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
// This handler provides:
//   - Transparent client interface (clients still use /chat/completions format)
//   - Request transformation (Chat Completions → Responses API)
//   - Conversation continuation (previous_response_id management)
//   - Multi-client broadcast (via StreamManager)
//   - Response ID tracking (stored in Firestore)
//
// Flow:
//  1. Fetch previous response_id from Firestore (for continuation)
//  2. Transform request: add store=true, previous_response_id if exists
//  3. Make HTTP request to OpenAI /responses endpoint
//  4. Use StreamManager for multi-client broadcast
//  5. Extract response_id from first chunk
//  6. Save response_id to Firestore when complete
//
// Parameters:
//   - c: Gin context
//   - requestBody: Original request body from client
//   - provider: Provider config (contains BaseURL, APIKey)
//   - model: Model ID (e.g., "gpt-5-pro")
//   - log: Logger
//   - trackingService: Request tracking service
//   - messageService: Message storage service (includes response_id storage)
//   - streamManager: Stream manager for broadcast
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
	streamManager *streaming.StreamManager,
	cfg *config.Config,
) error {
	// Validate required parameters
	if provider == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Provider configuration is nil"})
		return fmt.Errorf("provider is nil")
	}
	if streamManager == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Stream manager not initialized"})
		return fmt.Errorf("streamManager is nil")
	}
	if log == nil {
		// Can't log error if logger is nil, just return
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
			go titleService.QueueTitleGeneration(context.Background(), title_generation.TitleGenerationRequest{
				UserID:            userID,
				ChatID:            chatID,
				FirstMessage:      firstMessage,
				Model:             model,
				BaseURL:           provider.BaseURL,
				Platform:          platform,
				EncryptionEnabled: encryptionEnabled,
			}, provider.APIKey)
		}
	}

	// Save placeholder message immediately (before making request)
	if messageService != nil {
		// Extract encryption setting
		var encryptionEnabled *bool
		if val, exists := c.Get("encryptionEnabled"); exists {
			if boolPtr, ok := val.(*bool); ok {
				encryptionEnabled = boolPtr
			}
		}

		// Save placeholder synchronously (fast operation)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = messageService.SaveThinkingMessage(ctx, userID, chatID, messageID, model, encryptionEnabled)
		cancel()
	}

	// Step 3: Transform request for Responses API
	adapter := responses.NewAdapter()
	transformedBody, err := adapter.TransformRequest(requestBody, previousResponseID)
	if err != nil {
		log.Error("failed to transform request",
			slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to transform request for Responses API"})
		return fmt.Errorf("failed to transform request: %w", err)
	}

	// Make HTTP request to OpenAI /responses endpoint
	targetURL := provider.BaseURL + "/responses"
	req, err := http.NewRequestWithContext(context.Background(), "POST", targetURL, bytes.NewReader(transformedBody))
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
	req.Header.Set("Accept", "text/event-stream")

	// Make request
	client := &http.Client{
		Transport: proxyTransport,
		Timeout:   10 * time.Minute,
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Error("failed to make request to Responses API",
			slog.String("error", err.Error()),
			slog.String("target_url", targetURL))
		c.JSON(http.StatusBadGateway, gin.H{"error": "Failed to connect to Responses API"})
		return fmt.Errorf("failed to make request: %w", err)
	}

	// Check for errors
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Error("Responses API returned error", slog.Int("status_code", resp.StatusCode))
		c.Data(resp.StatusCode, "application/json", body)
		return fmt.Errorf("Responses API error: %d", resp.StatusCode)
	}

	// Step 5: Use StreamManager for multi-client broadcast
	// Get or create stream session
	session, isNew := streamManager.GetOrCreateSession(chatID, messageID, resp.Body)

	// Step 5.5: Register completion handler (BEFORE subscribing)
	// This ensures message gets saved even if client disconnects early
	if isNew && messageService != nil {
		// Extract encryption setting
		var encryptionEnabled *bool
		if val, exists := c.Get("encryptionEnabled"); exists {
			if boolPtr, ok := val.(*bool); ok {
				encryptionEnabled = boolPtr
			}
		}

		// Capture variables for goroutine
		capturedUserID := userID
		capturedChatID := chatID
		capturedModel := model
		capturedEncryption := encryptionEnabled

		// Start goroutine that waits for session completion
		// This runs independently of client connection
		go func() {
			// Wait for session to complete (blocking)
			session.WaitForCompletion()

			// Save response_id
			responseID := session.GetResponseID()
			if responseID != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := messageService.SaveResponseID(ctx, capturedUserID, capturedChatID, responseID); err != nil {
					log.Error("failed to save response_id", slog.String("error", err.Error()))
				}
				cancel()
			}

			// Save completed message
			err := streamManager.SaveCompletedSession(context.Background(), session, capturedUserID, capturedEncryption, capturedModel)
			if err != nil {
				log.Error("failed to save completed session", slog.String("error", err.Error()))
			}
		}()
	}

	// Subscribe to the session
	subscriberID := uuid.New().String()
	subscriber, err := session.Subscribe(c.Request.Context(), subscriberID, streaming.SubscriberOptions{
		ReplayFromStart: !isNew, // Replay from start if joining existing stream
		BufferSize:      100,
	})
	if err != nil {
		log.Error("failed to subscribe to Responses API stream", slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to subscribe to stream"})
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Record subscription metric
	streamManager.RecordSubscription()

	// Stream to client and extract response_id
	streamToClientWithResponseID(c, subscriber, session, log, adapter)

	// Log request to database
	logRequestToDatabaseWithProvider(c, trackingService, model, nil, provider.Name)

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
