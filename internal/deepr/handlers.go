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
func DeepResearchHandler(logger *logger.Logger, trackingService *request_tracking.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("deepr")

		// Get user ID from auth context
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			log.Error("user not authenticated")
			c.JSON(http.StatusUnauthorized, gin.H{"error": "User not authenticated"})
			return
		}

		// Get chat ID from query parameter
		chatID := c.Query("chat_id")
		if chatID == "" {
			log.Error("missing chat_id parameter")
			c.JSON(http.StatusBadRequest, gin.H{"error": "chat_id parameter is required"})
			return
		}

		// Upgrade HTTP connection to WebSocket
		conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			log.Error("failed to upgrade connection to websocket", slog.String("error", err.Error()))
			return
		}
		defer conn.Close()

		log.Info("websocket connection established",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))

		// Create service instance
		service := NewService(logger, trackingService)

		// Handle the WebSocket connection
		service.HandleConnection(c.Request.Context(), conn, userID, chatID)
	}
}
