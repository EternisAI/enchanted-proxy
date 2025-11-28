package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
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

		sessionKey := fmt.Sprintf("%s:%s", chatID, messageID)
		log.Info("stop request received",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("session_key", sessionKey),
			slog.String("user_id", userID))

		// Get existing session
		session := streamManager.GetSession(chatID, messageID)
		if session == nil {
			// Get metrics to see what sessions exist
			metrics := streamManager.GetMetrics()
			log.Error("stream not found",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID),
				slog.String("session_key", sessionKey),
				slog.Int("active_streams", metrics.ActiveStreams),
				slog.Int("completed_streams", metrics.CompletedStreams))
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
