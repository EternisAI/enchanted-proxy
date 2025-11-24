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
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		// Allow all origins for now (can restrict based on config)
		return true
	},
}

// ChatStreamHandler handles WebSocket connections for chat-level streaming.
// Endpoint: WS /api/v1/chats/:chatId/stream?replay=true
//
// The handler upgrades HTTP to WebSocket, authenticates the user, verifies chat ownership,
// and subscribes to all message streams in the chat.
func ChatStreamHandler(
	logger *logger.Logger,
	chatStreamManager *streaming.ChatStreamManager,
	firestoreClient *messaging.FirestoreClient,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("chat-stream-handler")

		// Extract user ID from auth
		userID, exists := auth.GetUserID(c)
		if !exists {
			log.Error("user ID not found in context")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		// Extract chat ID from path
		chatID := c.Param("chatId")
		if chatID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId is required"})
			return
		}

		// Validate chat ID length
		if len(chatID) > maxChatIDLength {
			log.Warn("chat ID too long",
				slog.String("chat_id_len", fmt.Sprintf("%d", len(chatID))))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chatId exceeds maximum length"})
			return
		}

		if firestoreClient != nil {
			err := firestoreClient.VerifyChatOwnership(c.Request.Context(), userID, chatID)
			if err != nil {
				if status.Code(err) == codes.PermissionDenied {
					c.JSON(http.StatusForbidden, gin.H{"error": "Forbidden: You don't own this chat"})
					return
				}
				log.Error("failed to verify chat ownership", slog.String("error", err.Error()))
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to verify permissions"})
				return
			}
		}

		replay := c.DefaultQuery("replay", "true") == "true"

		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Error("failed to upgrade connection", slog.String("error", err.Error()))
			return
		}

		subscriberID := uuid.New().String()
		hub := chatStreamManager.GetOrCreateHub(chatID)

		err = hub.Subscribe(c.Request.Context(), subscriberID, userID, conn, replay)
		if err != nil {
			log.Error("failed to subscribe to hub", slog.String("error", err.Error()))
			conn.Close()
			return
		}

		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error {
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			return nil
		})

		defer func() {
			hub.Unsubscribe(subscriberID)
			conn.Close()
		}()

		// Block in read loop to keep connection open
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("websocket read error", slog.String("error", err.Error()))
				}
				break
			}
		}
	}
}
