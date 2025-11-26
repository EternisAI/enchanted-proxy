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
) error {
	// Extract chat ID and message ID from headers
	chatID := c.GetHeader("X-Chat-ID")
	messageID := c.GetHeader("X-Message-ID")

	// If either is missing, generate fallback values
	if chatID == "" {
		chatID = uuid.New().String()
		log.Debug("generated fallback chat ID", slog.String("chat_id", chatID))
	}
	if messageID == "" {
		messageID = uuid.New().String()
		log.Debug("generated fallback message ID", slog.String("message_id", messageID))
	}

	log.Info("handling streaming response with broadcast",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.String("model", model))

	// Get or create stream session
	// If session exists, we'll join it (multi-client broadcast)
	// If session doesn't exist, we'll create it and start upstream read
	session, isNew := streamManager.GetOrCreateSession(chatID, messageID, resp.Body)

	// Set original request body and provider config on new sessions for tool execution
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
	}

	if !isNew {
		log.Info("joining existing stream session",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.Int("existing_subscribers", session.GetSubscriberCount()))
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
			err := streamManager.SaveCompletedSession(context.Background(), session, userID, encryptionEnabled)
			if err != nil {
				log.Error("failed to save completed session",
					slog.String("error", err.Error()),
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
			}
		}
	}

	// Log request to database (token usage)
	logRequestToDatabase(c, trackingService, model, nil) // TODO: Extract token usage from session

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

		logRequestToDatabase(c, trackingService, model, tokenUsage)

		// Save message to Firestore asynchronously
		isError := resp.StatusCode >= 400
		saveMessageAsync(c, messageService, fullContent.String(), isError)
	}()

	// Remove Content-Length for chunked encoding.
	resp.Header.Del("Content-Length")
	return nil
}
