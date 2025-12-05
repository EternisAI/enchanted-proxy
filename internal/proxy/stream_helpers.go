package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// streamToClient streams chunks from a subscriber to an HTTP client.
//
// This function:
//  1. Reads chunks from subscriber channel
//  2. Writes each chunk to client as SSE
//  3. Handles client disconnects gracefully
//  4. Unsubscribes when done
//
// Parameters:
//   - c: Gin context (HTTP response writer)
//   - subscriber: Stream subscriber with chunk channel
//   - session: The stream session (for unsubscribe)
//   - log: Logger for this operation
//
// The function blocks until:
//   - Stream completes (final chunk received)
//   - Client disconnects
//   - Error occurs
func streamToClient(c *gin.Context, subscriber *streaming.StreamSubscriber, session *streaming.StreamSession, log *logger.Logger) {
	defer func() {
		// Always unsubscribe when done
		session.Unsubscribe(subscriber.ID)
		log.Debug("client stream finished",
			slog.String("subscriber_id", subscriber.ID))
	}()

	// Set SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // Disable nginx buffering

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
				// Channel closed, stream completed
				log.Debug("subscriber channel closed",
					slog.String("subscriber_id", subscriber.ID))
				return
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

// saveCompletedStreamMessage saves a completed stream session's message to Firestore.
//
// This should be called immediately after a stream completes (successfully or stopped).
// It extracts the content from the session and queues it for async storage.
//
// Parameters:
//   - c: Gin context (for extracting user ID and encryption settings)
//   - session: The completed stream session
//   - messageService: Service for storing messages
//   - log: Logger for this operation
func saveCompletedStreamMessage(c *gin.Context, session *streaming.StreamSession, messageService *messaging.Service, log *logger.Logger) {
	if messageService == nil {
		return
	}

	// Extract user ID
	userID, exists := auth.GetUserID(c)
	if !exists {
		log.Error("cannot save message: user ID not found")
		return
	}

	// Extract chat ID and message ID from headers
	chatID := c.GetHeader("X-Chat-ID")
	if chatID == "" {
		log.Debug("skipping message save: X-Chat-ID header not provided")
		return
	}

	messageID := c.GetHeader("X-Message-ID")
	if messageID == "" {
		// Generate fallback message ID
		messageID = uuid.New().String()
		log.Debug("generated fallback message ID", slog.String("message_id", messageID))
	}

	// Extract encryption setting
	var encryptionEnabled *bool
	if val, exists := c.Get("encryptionEnabled"); exists {
		if boolPtr, ok := val.(*bool); ok {
			encryptionEnabled = boolPtr
		}
	}

	// Extract content from session
	content := session.GetContent()
	if content == "" {
		log.Debug("skipping message save: no content extracted from stream")
		return
	}

	// Check if stopped
	stopped := session.IsStopped()
	stoppedBy, stopReason := session.GetStopInfo()

	log.Info("saving completed stream message",
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.Int("content_length", len(content)),
		slog.Bool("stopped", stopped),
		slog.String("stopped_by", stoppedBy),
		slog.String("stop_reason", string(stopReason)))

	// Build message with stop metadata
	msg := messaging.MessageToStore{
		UserID:            userID,
		ChatID:            chatID,
		MessageID:         messageID,
		IsFromUser:        false, // AI response
		Content:           content,
		IsError:           session.GetError() != nil,
		EncryptionEnabled: encryptionEnabled,
		Stopped:           stopped,
		StoppedBy:         stoppedBy,
		StopReason:        string(stopReason),
	}

	// Store asynchronously (with background context - shouldn't be tied to request)
	if err := messageService.StoreMessageAsync(context.Background(), msg); err != nil {
		log.Error("failed to queue message for storage",
			slog.String("error", err.Error()),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))
	}
}

// makeSessionKey creates a session key from chat ID and message ID.
// Format: "chatID:messageID"
func makeSessionKey(chatID, messageID string) string {
	return fmt.Sprintf("%s:%s", chatID, messageID)
}

// extractRequestInfo extracts routing information from the request.
// Returns chat ID, message ID, model, and user ID.
func extractRequestInfo(c *gin.Context, requestBody []byte) (chatID, messageID, model, userID string, err error) {
	// Extract from headers
	chatID = c.GetHeader("X-Chat-ID")
	messageID = c.GetHeader("X-Message-ID")

	// Extract model from body
	model = ExtractModelFromRequestBody(c.Request.URL.Path, requestBody)

	// Extract user ID from auth
	userID, exists := auth.GetUserID(c)
	if !exists {
		return "", "", "", "", fmt.Errorf("user ID not found in context")
	}

	return chatID, messageID, model, userID, nil
}

// prepareUpstreamRequest creates the HTTP request to forward to the AI provider.
//
// Parameters:
//   - baseURL: Provider base URL (e.g., "https://api.openai.com/v1")
//   - path: Request path (e.g., "/chat/completions")
//   - requestBody: Original request body bytes
//   - apiKey: API key for authorization
//   - c: Gin context (for copying headers)
//
// Returns:
//   - *http.Request: The prepared request
//   - error: If request creation failed
//
// Note: This function creates a request with background context, ensuring
// the upstream request completes even if the client disconnects.
func prepareUpstreamRequest(baseURL, path string, requestBody []byte, apiKey string, c *gin.Context) (*http.Request, error) {
	// Build full URL
	targetURL := baseURL + path

	// Create body reader from bytes
	// We create a new reader because c.Request.Body may already be consumed
	var bodyReader io.Reader
	if len(requestBody) > 0 {
		bodyReader = bytes.NewReader(requestBody)
	}

	// Create request with background context (independent of client)
	// This ensures upstream request completes even if client disconnects
	req, err := http.NewRequestWithContext(context.Background(), c.Request.Method, targetURL, io.NopCloser(bodyReader))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	// Set authorization
	req.Header.Set("Authorization", "Bearer "+apiKey)

	// Copy relevant headers from original request
	if userAgent := c.Request.Header.Get("User-Agent"); userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	} else {
		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	}

	// Set content type
	if contentType := c.Request.Header.Get("Content-Type"); contentType != "" {
		req.Header.Set("Content-Type", contentType)
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	// Disable gzip compression (prevents proxy from having to decompress/recompress)
	req.Header.Set("Accept-Encoding", "identity")

	// Set content length
	if len(requestBody) > 0 {
		req.ContentLength = int64(len(requestBody))
	}

	return req, nil
}
