package deepr

import (
	"log/slog"
	"net/http"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

// DeepResearchHandler handles WebSocket connections for deep research streaming
func DeepResearchHandler(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("deepr")

		log.Info("websocket connection request received",
			slog.String("path", c.Request.URL.Path),
			slog.String("remote_addr", c.Request.RemoteAddr),
			slog.String("user_agent", c.Request.UserAgent()),
			slog.String("method", c.Request.Method))

		// Get user ID from auth context
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			log.Error("authentication failed - user not found in context",
				slog.String("path", c.Request.URL.Path),
				slog.String("remote_addr", c.Request.RemoteAddr))
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		log.Info("user authenticated",
			slog.String("user_id", userID))

		// Get chat ID from query parameter
		chatID := c.Query("chat_id")
		if chatID == "" {
			log.Error("missing required parameter",
				slog.String("user_id", userID),
				slog.String("parameter", "chat_id"))
			c.JSON(http.StatusBadRequest, gin.H{"error": "chat_id parameter is required"})
			return
		}

		log.Info("session parameters validated",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))

		// Upgrade HTTP connection to WebSocket
		log.Info("upgrading connection to websocket",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Error("websocket upgrade failed",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("error", err.Error()))
			return
		}
		defer conn.Close()

		log.Info("websocket connection established successfully",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("remote_addr", c.Request.RemoteAddr))

		// Create service instance with database storage
		service := NewService(logger, trackingService, firebaseClient, storage)

		// Handle the WebSocket connection
		service.HandleConnection(c.Request.Context(), conn, userID, chatID)
	}
}
