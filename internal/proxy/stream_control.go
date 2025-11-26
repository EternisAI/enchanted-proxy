package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/streaming"
	"github.com/gin-gonic/gin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// Maximum length for chat and message IDs to prevent memory abuse
	maxChatIDLength    = 256
	maxMessageIDLength = 256
)

// StopStreamHandler handles POST /api/v1/chats/:chatId/messages/:messageId/stop
// Stops an in-progress AI response generation
func StopStreamHandler(
	logger *logger.Logger,
	streamManager *streaming.StreamManager,
	firestoreClient *messaging.FirestoreClient,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("stream-control")

		// Extract user ID from auth
		userID, exists := auth.GetUserID(c)
		if !exists {
			log.Error("user ID not found in context")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Extract path parameters
		chatID := c.Param("chatId")
		messageID := c.Param("messageId")

		// Validate parameters
		if chatID == "" || messageID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId and messageId are required"})
			return
		}

		// Input validation: Check length limits
		if len(chatID) > maxChatIDLength || len(messageID) > maxMessageIDLength {
			log.Warn("ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))),
				slog.String("message_id_len", fmt.Sprintf("%d", len(messageID))))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId or messageId exceeds maximum length"})
			return
		}

		// Authorization: Verify user owns this chat
		if firestoreClient != nil {
			err := firestoreClient.VerifyChatOwnership(c.Request.Context(), userID, chatID)
			if err != nil {
				if status.Code(err) == codes.PermissionDenied {
					log.Warn("chat ownership verification failed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID))
					c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: You don't own this chat"})
					return
				}
				log.Error("failed to verify chat ownership",
					slog.String("error", err.Error()),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify permissions"})
				return
			}
		}

		log.Info("stop request received",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("user_id", userID))

		// Get existing session
		session := streamManager.GetSession(chatID, messageID)
		if session == nil {
			log.Error("stream not found",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			c.JSON(http.StatusNotFound, gin.H{
				"error":      "Stream not found",
				"message_id": messageID,
			})
			return
		}

		// Check if already completed
		if session.IsCompleted() {
			log.Error("stream already completed",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			c.JSON(http.StatusConflict, gin.H{
				"error":      "Stream already completed",
				"message_id": messageID,
				"completed":  true,
			})
			return
		}

		// Stop the stream
		err := session.Stop(userID, streaming.StopReasonUserCancelled)
		if err != nil {
			// Check if stream was already stopped (concurrent stop requests)
			if err.Error() == "stream already stopped" {
				log.Warn("stream already stopped",
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
				c.JSON(http.StatusConflict, gin.H{
					"error":      "Stream already stopped",
					"message_id": messageID,
					"stopped":    true,
				})
				return
			}

			log.Error("failed to stop stream",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":      "Failed to stop stream",
				"details":    err.Error(),
				"message_id": messageID,
			})
			return
		}

		// Get info about stopped stream
		info := session.GetInfo()
		chunks := session.GetStoredChunks()

		log.Info("stream stopped successfully",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.Int("chunks_generated", len(chunks)))

		// Return success response
		c.JSON(http.StatusOK, gin.H{
			"stopped":                true,
			"message_id":             messageID,
			"chunks_generated":       len(chunks),
			"stopped_at":             time.Now().UTC().Format(time.RFC3339),
			"partial_content_stored": len(chunks) > 0,
			"subscriber_count":       info.SubscriberCount,
		})
	}
}

// GetStreamHandler handles GET /api/v1/chats/:chatId/messages/:messageId/stream
// Subscribes to an existing stream (active or completed)
func GetStreamHandler(
	logger *logger.Logger,
	streamManager *streaming.StreamManager,
	firestoreClient *messaging.FirestoreClient,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("stream-subscribe")

		// Extract user ID from auth
		userID, exists := auth.GetUserID(c)
		if !exists {
			log.Error("user ID not found in context")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Extract path parameters
		chatID := c.Param("chatId")
		messageID := c.Param("messageId")

		// Validate parameters
		if chatID == "" || messageID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId and messageId are required"})
			return
		}

		// Input validation: Check length limits
		if len(chatID) > maxChatIDLength || len(messageID) > maxMessageIDLength {
			log.Warn("ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))),
				slog.String("message_id_len", fmt.Sprintf("%d", len(messageID))))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId or messageId exceeds maximum length"})
			return
		}

		// Authorization: Verify user owns this chat
		if firestoreClient != nil {
			err := firestoreClient.VerifyChatOwnership(c.Request.Context(), userID, chatID)
			if err != nil {
				if status.Code(err) == codes.PermissionDenied {
					log.Warn("chat ownership verification failed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID))
					c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: You don't own this chat"})
					return
				}
				log.Error("failed to verify chat ownership",
					slog.String("error", err.Error()),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify permissions"})
				return
			}
		}

		// Parse query parameters
		replayParam := c.DefaultQuery("replay", "true")
		replay, _ := strconv.ParseBool(replayParam)

		waitParam := c.DefaultQuery("wait_for_completion", "false")
		waitForCompletion, _ := strconv.ParseBool(waitParam)

		log.Info("stream subscribe request",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("user_id", userID),
			slog.Bool("replay", replay),
			slog.Bool("wait", waitForCompletion))

		// Try to get existing session
		session := streamManager.GetSession(chatID, messageID)

		// If not found, optionally wait for it to start
		if session == nil && waitForCompletion {
			log.Info("waiting for stream to start",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))

			// Poll for session with timeout
			ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
			defer cancel()

			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					log.Error("timeout waiting for stream",
						slog.String("chat_id", chatID),
						slog.String("message_id", messageID))
					c.JSON(http.StatusNotFound, gin.H{
						"error":      "Stream not found (timeout)",
						"message_id": messageID,
					})
					return
				case <-ticker.C:
					session = streamManager.GetSession(chatID, messageID)
					if session != nil {
						goto sessionFound
					}
				}
			}
		}

	sessionFound:
		if session == nil {
			log.Error("stream not found",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			c.JSON(http.StatusNotFound, gin.H{
				"error":      "Stream not found",
				"message_id": messageID,
			})
			return
		}

		// Subscribe to session
		subscriberID := fmt.Sprintf("subscriber-%s-%d", userID, time.Now().UnixNano())
		opts := streaming.SubscriberOptions{
			ReplayFromStart: replay,
			BufferSize:      100,
		}

		subscriber, err := session.Subscribe(c.Request.Context(), subscriberID, opts)
		if err != nil {
			log.Error("failed to subscribe",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			c.JSON(http.StatusInternalServerError, gin.H{
				"error":   "Failed to subscribe to stream",
				"details": err.Error(),
			})
			return
		}

		log.Info("subscriber created",
			slog.String("subscriber_id", subscriberID),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))

		// Ensure cleanup on connection close
		defer func() {
			session.Unsubscribe(subscriberID)
			log.Info("subscriber disconnected",
				slog.String("subscriber_id", subscriberID),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
		}()

		// Set SSE headers
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")

		// Get the response writer and flush
		w := c.Writer
		flusher, ok := w.(http.Flusher)
		if !ok {
			log.Error("response writer does not support flushing")
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "Streaming not supported",
			})
			return
		}

		// Stream chunks to client
		for chunk := range subscriber.Ch {
			// Write chunk
			_, err := fmt.Fprintf(w, "%s\n\n", chunk.Line)
			if err != nil {
				log.Error("failed to write chunk",
					slog.String("error", err.Error()),
					slog.String("subscriber_id", subscriberID))
				return
			}
			flusher.Flush()

			// Check if final chunk
			if chunk.IsFinal {
				log.Info("stream completed",
					slog.String("subscriber_id", subscriberID),
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
				return
			}
		}

		log.Info("stream channel closed",
			slog.String("subscriber_id", subscriberID))
	}
}

// StreamStatusHandler handles GET /api/v1/chats/:chatId/messages/:messageId/status
// Returns status information about a stream without subscribing
func StreamStatusHandler(
	logger *logger.Logger,
	streamManager *streaming.StreamManager,
	firestoreClient *messaging.FirestoreClient,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("stream-status")

		// Extract user ID from auth
		userID, exists := auth.GetUserID(c)
		if !exists {
			log.Error("user ID not found in context")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Extract path parameters
		chatID := c.Param("chatId")
		messageID := c.Param("messageId")

		// Validate parameters
		if chatID == "" || messageID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId and messageId are required"})
			return
		}

		// Input validation: Check length limits
		if len(chatID) > maxChatIDLength || len(messageID) > maxMessageIDLength {
			log.Warn("ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))),
				slog.String("message_id_len", fmt.Sprintf("%d", len(messageID))))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId or messageId exceeds maximum length"})
			return
		}

		// Authorization: Verify user owns this chat
		if firestoreClient != nil {
			err := firestoreClient.VerifyChatOwnership(c.Request.Context(), userID, chatID)
			if err != nil {
				if status.Code(err) == codes.PermissionDenied {
					log.Warn("chat ownership verification failed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID))
					c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: You don't own this chat"})
					return
				}
				log.Error("failed to verify chat ownership",
					slog.String("error", err.Error()),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify permissions"})
				return
			}
		}

		log.Debug("stream status request",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("user_id", userID))

		// Get existing session
		session := streamManager.GetSession(chatID, messageID)
		if session == nil {
			c.JSON(http.StatusNotFound, gin.H{
				"exists":     false,
				"message_id": messageID,
			})
			return
		}

		// Get session info
		info := session.GetInfo()
		chunks := session.GetStoredChunks()
		content := session.GetContent()

		response := gin.H{
			"exists":           true,
			"message_id":       info.MessageID,
			"chat_id":          info.ChatID,
			"completed":        info.Completed,
			"subscriber_count": info.SubscriberCount,
			"chunks_count":     len(chunks),
			"content_length":   len(content),
			"started_at":       info.StartTime.Format(time.RFC3339),
		}

		if info.Completed {
			duration := time.Since(info.StartTime)
			response["duration_ms"] = duration.Milliseconds()
		}

		if session.IsStopped() {
			stoppedBy, reason := session.GetStopInfo()
			response["stopped"] = true
			response["stopped_by"] = stoppedBy
			response["stop_reason"] = reason
		}

		c.JSON(http.StatusOK, response)
	}
}

// ActiveStreamHandler handles GET /api/v1/chats/:chatId/active-stream
// Returns information about the active stream for a chat, if one exists
func ActiveStreamHandler(
	logger *logger.Logger,
	streamManager *streaming.StreamManager,
	firestoreClient *messaging.FirestoreClient,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("active-stream")

		// Extract user ID from auth
		userID, exists := auth.GetUserID(c)
		if !exists {
			log.Error("user ID not found in context")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Extract path parameter
		chatID := c.Param("chatId")

		// Validate parameter
		if chatID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
			return
		}

		// Input validation: Check length limits
		if len(chatID) > maxChatIDLength {
			log.Warn("chat ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId exceeds maximum length"})
			return
		}

		// Authorization: Verify user owns this chat
		if firestoreClient != nil {
			err := firestoreClient.VerifyChatOwnership(c.Request.Context(), userID, chatID)
			if err != nil {
				if status.Code(err) == codes.PermissionDenied {
					log.Warn("chat ownership verification failed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID))
					c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: You don't own this chat"})
					return
				}
				log.Error("failed to verify chat ownership",
					slog.String("error", err.Error()),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify permissions"})
				return
			}
		}

		log.Debug("active stream request",
			slog.String("chat_id", chatID),
			slog.String("user_id", userID))

		// Get active stream for this chat
		streamInfo := streamManager.GetActiveStreamForChat(chatID)
		if streamInfo == nil {
			c.JSON(http.StatusOK, gin.H{
				"exists": false,
			})
			return
		}

		// Get stored chunks count for the session
		session := streamManager.GetSession(streamInfo.ChatID, streamInfo.MessageID)
		var chunksCount int
		if session != nil {
			chunks := session.GetStoredChunks()
			chunksCount = len(chunks)
		}

		log.Info("active stream found",
			slog.String("chat_id", chatID),
			slog.String("message_id", streamInfo.MessageID),
			slog.Int("subscriber_count", streamInfo.SubscriberCount),
			slog.Int("chunks_count", chunksCount))

		// Return stream information
		c.JSON(http.StatusOK, gin.H{
			"exists":           true,
			"message_id":       streamInfo.MessageID,
			"started_at":       streamInfo.StartTime.Format(time.RFC3339),
			"chunks_count":     chunksCount,
			"subscriber_count": streamInfo.SubscriberCount,
		})
	}
}
