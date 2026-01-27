package proxy

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
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
			errors.Unauthorized(c, "Authentication required", nil)
			return
		}

		// Extract path parameters
		chatID := c.Param("chatId")
		messageID := c.Param("messageId")

		// Validate parameters
		if chatID == "" || messageID == "" {
			errors.BadRequest(c, "chatId and messageId are required", nil)
			return
		}

		// Input validation: Check length limits
		if len(chatID) > maxChatIDLength || len(messageID) > maxMessageIDLength {
			log.Warn("ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))),
				slog.String("message_id_len", fmt.Sprintf("%d", len(messageID))))
			errors.BadRequest(c, "chatId or messageId exceeds maximum length", nil)
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
					errors.AbortWithForbidden(c, errors.ChatNotOwned(chatID))
					return
				}
				log.Error("failed to verify chat ownership",
					slog.String("error", err.Error()),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
				errors.Internal(c, "Failed to verify permissions", nil)
				return
			}
		}

		sessionKey := fmt.Sprintf("%s:%s", chatID, messageID)
		log.Info("stop request received",
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.String("session_key", sessionKey),
			slog.String("user_id", userID))

		// Get existing session (local lookup first)
		session := streamManager.GetSession(chatID, messageID)
		if session == nil {
			// Session not on this instance - try distributed cancel via NATS
			distCancel := streamManager.GetDistributedCancel()
			if distCancel == nil {
				log.Warn("distributed cancel not available, cannot reach other instances",
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))
			} else {
				log.Info("session not local, attempting distributed cancel",
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID))

				resp, err := distCancel.RequestCancel(c.Request.Context(), chatID, messageID, userID)
				if err != nil {
					log.Error("distributed cancel failed",
						slog.String("error", err.Error()),
						slog.String("chat_id", chatID),
						slog.String("message_id", messageID))
					errors.Internal(c, "Failed to stop stream", map[string]interface{}{
						"details":    err.Error(),
						"message_id": messageID,
					})
					return
				}

				// Handle distributed cancel response
				if resp.Found {
					if resp.Success {
						log.Info("stream stopped via distributed cancel",
							slog.String("chat_id", chatID),
							slog.String("message_id", messageID),
							slog.String("remote_instance", resp.InstanceID),
							slog.Int("chunks_generated", resp.ChunksGenerated))
						c.JSON(http.StatusOK, gin.H{
							"stopped":          true,
							"message_id":       messageID,
							"chunks_generated": resp.ChunksGenerated,
							"stopped_at":       time.Now().UTC().Format(time.RFC3339),
							"remote_instance":  resp.InstanceID,
						})
						return
					}

					// Found but couldn't stop
					if resp.AlreadyComplete {
						errors.Conflict(c, "Stream already completed", map[string]interface{}{
							"message_id": messageID,
							"completed":  true,
						})
						return
					}
					if resp.AlreadyStopped {
						errors.Conflict(c, "Stream already stopped", map[string]interface{}{
							"message_id": messageID,
							"stopped":    true,
						})
						return
					}
					errors.Internal(c, "Failed to stop stream", map[string]interface{}{
						"details":    resp.Error,
						"message_id": messageID,
					})
					return
				}
			}

			// Not found locally or via distributed cancel
			metrics := streamManager.GetMetrics()
			log.Error("stream not found",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID),
				slog.String("session_key", sessionKey),
				slog.Int("active_streams", metrics.ActiveStreams),
				slog.Int("completed_streams", metrics.CompletedStreams))
			errors.NotFound(c, "Stream not found", map[string]interface{}{
				"message_id": messageID,
			})
			return
		}

		// Check if already completed
		if session.IsCompleted() {
			log.Error("stream already completed",
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			errors.Conflict(c, "Stream already completed", map[string]interface{}{
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
				errors.Conflict(c, "Stream already stopped", map[string]interface{}{
					"message_id": messageID,
					"stopped":    true,
				})
				return
			}

			log.Error("failed to stop stream",
				slog.String("error", err.Error()),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID))
			errors.Internal(c, "Failed to stop stream", map[string]interface{}{
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
