package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/eternisai/enchanted-proxy/internal/routing"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleStreamingWithBroadcast handles streaming responses with multi-client broadcast support.
//
// This is the new streaming path that:
//  1. Creates or joins existing stream sessions (multi-client broadcast)
//  2. Uses StreamManager for session lifecycle
//  3. Continues reading upstream even if all clients disconnect
//  4. Saves complete message to Firestore
//
// Parameters:
//   - c: Gin context
//   - resp: Upstream HTTP response
//   - log: Logger
//   - model: Model ID from request
//   - upstreamLatency: Time to first byte from upstream
//   - trackingService: Request tracking service
//   - messageService: Message storage service
//   - streamManager: Stream manager for broadcast
//   - cfg: Application configuration
//
// Returns:
//   - error: If handling failed
func handleStreamingWithBroadcast(
	c *gin.Context,
	resp *http.Response,
	log *logger.Logger,
	model string,
	upstreamLatency time.Duration,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	streamManager *streaming.StreamManager,
	cfg *config.Config,
	provider *routing.ProviderConfig,
) error {
	// Extract chat ID and message ID from headers
	chatID := c.GetHeader("X-Chat-ID")
	messageID := c.GetHeader("X-Message-ID")

	// If headers are missing, try to get from request body (stored in context by ProxyHandler)
	if chatID == "" {
		if bodyID, exists := c.Get("bodyChatId"); exists {
			if idStr, ok := bodyID.(string); ok && idStr != "" {
				chatID = idStr
				log.Info("using chatId from request body (header missing)")
			}
		}
	}
	if messageID == "" {
		if bodyID, exists := c.Get("bodyMessageId"); exists {
			if idStr, ok := bodyID.(string); ok && idStr != "" {
				messageID = idStr
				log.Info("using messageId from request body (header missing)")
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

	log.Info("Chat Completions request headers",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.String("model", model))

	// Check if pending session exists (created before HTTP request)
	session := streamManager.GetSession(chatID, messageID)
	var isNew bool

	if session != nil {
		// Check if this is a pending session (no upstream body yet) or an existing active stream
		if session.IsStarted() {
			// Already streaming - this is a late joiner
			log.Info("joining existing active stream session",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID),
				slog.Int("existing_subscribers", session.GetSubscriberCount()))
			isNew = false
		} else {
			// Pending session - attach upstream body and start reading
			log.Debug("attaching upstream body to pending session",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			session.SetUpstreamBodyAndStart(resp.Body)
			isNew = true // First time starting this session
		}
	} else {
		// No session at all - create new one (shouldn't happen often with pending session creation)
		log.Debug("creating new session with upstream body (no pending session found)",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))
		session, isNew = streamManager.GetOrCreateSession(chatID, messageID, resp.Body)
	}

	// Set original request body and provider config (needed for tool execution and continuation)
	// Do this for all new sessions (whether created now or earlier as pending)
	if isNew {
		if requestBody, exists := c.Get("originalRequestBody"); exists {
			if bodyBytes, ok := requestBody.([]byte); ok {
				session.SetOriginalRequest(bodyBytes)
				log.Debug("set original request body for tool execution",
					slog.Int("body_size", len(bodyBytes)))
			}
		}

		// Set provider config for continuation requests
		if upstreamURL, exists := c.Get("upstreamURL"); exists {
			if urlStr, ok := upstreamURL.(string); ok {
				session.SetUpstreamURL(urlStr)
			}
		}
		if apiKey, exists := c.Get("upstreamAPIKey"); exists {
			if keyStr, ok := apiKey.(string); ok {
				session.SetUpstreamAPIKey(keyStr)
			}
		}

		// For GPT-5 Pro, save placeholder message immediately to allow client reconnection
		// This creates a "thinking" message in Firestore before streaming starts
		if model == "gpt-5-pro" && messageService != nil {
			userID, exists := auth.GetUserID(c)
			if exists {
				// Extract encryption setting
				var encryptionEnabled *bool
				if val, exists := c.Get("encryptionEnabled"); exists {
					if boolPtr, ok := val.(*bool); ok {
						encryptionEnabled = boolPtr
					}
				}

				// Save synchronously (fast operation, avoids race with completion save)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = messageService.SaveThinkingMessage(ctx, userID, chatID, messageID, model, encryptionEnabled)
				cancel()
			}
		}
	}

	// Subscribe to the session
	// ReplayFromStart=true for late joiners ensures they get the full response
	subscriberID := uuid.New().String()
	subscriber, err := session.Subscribe(c.Request.Context(), subscriberID, streaming.SubscriberOptions{
		ReplayFromStart: !isNew, // Replay from start if joining existing stream
		BufferSize:      100,
	})
	if err != nil {
		log.Error("failed to subscribe to stream",
			slog.String("error", err.Error()),
			slog.String("chat_id", chatID))
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Record subscription metric
	streamManager.RecordSubscription()

	// Stream to client (blocks until done)
	streamToClient(c, subscriber, session, log)

	// After streaming completes, save message if this was a new session
	// (Only the first subscriber saves to avoid duplicates)
	if isNew && session.IsCompleted() {
		// Extract encryption setting
		var encryptionEnabled *bool
		if val, exists := c.Get("encryptionEnabled"); exists {
			if boolPtr, ok := val.(*bool); ok {
				encryptionEnabled = boolPtr
			}
		}

		// Extract user ID
		userID, exists := auth.GetUserID(c)
		if exists {
			// Save completed session to Firestore
			err := streamManager.SaveCompletedSession(context.Background(), session, userID, encryptionEnabled, model)
			if err != nil {
				log.Error("failed to save completed session",
					slog.String("error", err.Error()),
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
			}
		}
	}

	// Log request to database with token usage
	// Retrieve token usage from completed session
	var tokenUsage *Usage
	if sessionUsage := session.GetTokenUsage(); sessionUsage != nil {
		tokenUsage = &Usage{
			PromptTokens:     sessionUsage.PromptTokens,
			CompletionTokens: sessionUsage.CompletionTokens,
			TotalTokens:      sessionUsage.TotalTokens,
		}
		log.Debug("logging request with token usage from session",
			slog.Int("prompt_tokens", tokenUsage.PromptTokens),
			slog.Int("completion_tokens", tokenUsage.CompletionTokens),
			slog.Int("total_tokens", tokenUsage.TotalTokens))
	} else {
		log.Warn("no token usage available from session",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))
	}

	// Log with multiplier if provider is available
	if provider != nil {
		logRequestToDatabaseWithProvider(c, trackingService, model, tokenUsage, provider.Name, provider.TokenMultiplier)
	} else {
		logRequestToDatabase(c, trackingService, model, tokenUsage)
	}

	return nil
}

// handleStreamingLegacy handles streaming responses the old way (no broadcast).
//
// This is the legacy streaming path that:
//   - Ties upstream reading to client connection
//   - Stops reading if client disconnects
//   - Each client gets its own upstream request
//
// This is kept for backward compatibility during migration.
func handleStreamingLegacy(
	resp *http.Response,
	log *logger.Logger,
	model string,
	upstreamLatency time.Duration,
	c *gin.Context,
	trackingService *request_tracking.Service,
	messageService *messaging.Service,
	provider *routing.ProviderConfig,
) error {
	pr, pw := io.Pipe()
	originalBody := resp.Body
	resp.Body = pr

	go func() {
		defer pw.Close()           //nolint:errcheck
		defer originalBody.Close() //nolint:errcheck

		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in streaming response handler",
					slog.Any("panic", r),
					slog.String("target_url", resp.Request.URL.String()),
					slog.String("provider", request_tracking.GetProviderFromBaseURL(c.GetHeader("X-BASE-URL"))),
				)
			}
		}()

		var reader io.Reader = originalBody

		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024) // 64KB initial, 1MB max.
		var tokenUsage *Usage
		var firstChunk string
		var fullContent strings.Builder // Accumulate full response content

		ctx := c.Request.Context()
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				log.Debug("client disconnected, stopping stream processing")
				return
			default:
			}

			line := scanner.Text()

			// Pipe the line to the client immediately.
			if _, err := pw.Write(append([]byte(line), '\n')); err != nil {
				log.Error("failed to write to pipe", slog.String("error", err.Error()))
				return
			}

			if firstChunk == "" && log.Enabled(ctx, slog.LevelDebug) && strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
				firstChunk = line
			}

			// Extract and accumulate content for message storage
			if content := extractContentFromSSELine(line); content != "" {
				fullContent.WriteString(content)
			}

			// Extract the token usage from second to last chunk which contains a usage field.
			// See: https://openrouter.ai/docs/use-cases/usage-accounting#streaming-with-usage-information
			if usage := extractTokenUsageFromSSELine(line); usage != nil {
				tokenUsage = usage
			}
		}

		if err := scanner.Err(); err != nil {
			log.Error("scanner error while processing SSE stream", slog.String("error", err.Error()))
		}

		logProxyResponse(log, resp, true, upstreamLatency, model, tokenUsage, []byte(firstChunk), c.Request.Context())

		// Log request to database with multiplier if provider is available
		if provider != nil {
			logRequestToDatabaseWithProvider(c, trackingService, model, tokenUsage, provider.Name, provider.TokenMultiplier)
		} else {
			logRequestToDatabase(c, trackingService, model, tokenUsage)
		}

		// Save message to Firestore asynchronously
		isError := resp.StatusCode >= 400
		saveMessageAsync(c, messageService, fullContent.String(), isError)
	}()

	// Remove Content-Length for chunked encoding.
	resp.Header.Del("Content-Length")
	return nil
}
