package deepr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

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

// StartDeepResearchRequest represents the request body for starting deep research.
type StartDeepResearchRequest struct {
	Query  string `json:"query" binding:"required"`
	ChatID string `json:"chat_id" binding:"required"`
}

// StartDeepResearchResponse represents the response for starting deep research.
type StartDeepResearchResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// StartDeepResearchHandler handles POST requests to start deep research.
func StartDeepResearchHandler(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage, sessionManager *SessionManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := logger.WithContext(c.Request.Context()).WithComponent("deepr")

		log.Info("deep research start request received",
			slog.String("path", c.Request.URL.Path),
			slog.String("remote_addr", c.Request.RemoteAddr),
			slog.String("method", c.Request.Method))

		// Get user ID from auth context
		userID, exists := auth.GetUserUUID(c)
		if !exists {
			log.Error("authentication failed - user not found in context",
				slog.String("path", c.Request.URL.Path),
				slog.String("remote_addr", c.Request.RemoteAddr))
			c.JSON(http.StatusUnauthorized, StartDeepResearchResponse{
				Success: false,
				Error:   "User not authenticated",
			})
			return
		}

		// Parse request body
		var req StartDeepResearchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			log.Error("invalid request body",
				slog.String("user_id", userID),
				slog.String("error", err.Error()))
			c.JSON(http.StatusBadRequest, StartDeepResearchResponse{
				Success: false,
				Error:   "Invalid request body: " + err.Error(),
			})
			return
		}

		log.Info("starting deep research",
			slog.String("user_id", userID),
			slog.String("chat_id", req.ChatID),
			slog.String("query", req.Query))

		// Create service instance
		service := NewService(logger, trackingService, firebaseClient, storage, sessionManager)

		// Check if there's already an active session
		if sessionManager.HasActiveBackend(userID, req.ChatID) {
			log.Info("active session already exists",
				slog.String("user_id", userID),
				slog.String("chat_id", req.ChatID))
			c.JSON(http.StatusOK, StartDeepResearchResponse{
				Success: true,
				Message: "Deep research session already active",
			})
			return
		}

		// Validate freemium access
		dummyConn := &websocket.Conn{} // Dummy connection for validation
		if err := service.validateFreemiumAccess(c.Request.Context(), dummyConn, userID, req.ChatID, false); err != nil {
			log.Error("freemium validation failed",
				slog.String("user_id", userID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			c.JSON(http.StatusForbidden, StartDeepResearchResponse{
				Success: false,
				Error:   err.Error(),
			})
			return
		}

		// Connect to deep research backend
		deepResearchHost := os.Getenv("DEEP_RESEARCH_WS")
		if deepResearchHost == "" {
			deepResearchHost = "localhost:3031"
		}

		wsURL := url.URL{
			Scheme: "ws",
			Host:   deepResearchHost,
			Path:   "/deep_research/" + userID + "/" + req.ChatID + "/",
		}

		log.Info("connecting to deep research backend",
			slog.String("user_id", userID),
			slog.String("chat_id", req.ChatID),
			slog.String("url", wsURL.String()))

		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = 30 * time.Second

		backendConn, _, err := dialer.Dial(wsURL.String(), nil)
		if err != nil {
			log.Error("failed to connect to deep research backend",
				slog.String("user_id", userID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			c.JSON(http.StatusServiceUnavailable, StartDeepResearchResponse{
				Success: false,
				Error:   "Failed to connect to deep research service",
			})
			return
		}

		// Create session context
		sessionCtx, cancel := context.WithCancel(context.Background())

		// Create and register session
		session := sessionManager.CreateSession(userID, req.ChatID, backendConn, sessionCtx, cancel)

		// Update backend connection status in storage
		if storage != nil {
			if err := storage.UpdateBackendConnectionStatus(userID, req.ChatID, true); err != nil {
				log.Error("failed to update backend connection status",
					slog.String("user_id", userID),
					slog.String("chat_id", req.ChatID),
					slog.String("error", err.Error()))
			}
		}

		// Send initial query to backend
		queryMsg := Request{
			Query: req.Query,
			Type:  "query",
		}
		queryJSON, err := json.Marshal(queryMsg)
		if err != nil {
			log.Error("failed to marshal query",
				slog.String("user_id", userID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			sessionManager.RemoveSession(userID, req.ChatID)
			backendConn.Close()
			c.JSON(http.StatusInternalServerError, StartDeepResearchResponse{
				Success: false,
				Error:   "Failed to prepare query",
			})
			return
		}

		if err := backendConn.WriteMessage(websocket.TextMessage, queryJSON); err != nil {
			log.Error("failed to send query to backend",
				slog.String("user_id", userID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			sessionManager.RemoveSession(userID, req.ChatID)
			backendConn.Close()
			c.JSON(http.StatusInternalServerError, StartDeepResearchResponse{
				Success: false,
				Error:   "Failed to send query to deep research service",
			})
			return
		}

		log.Info("deep research started successfully",
			slog.String("user_id", userID),
			slog.String("chat_id", req.ChatID))

		// Start goroutine to handle backend messages
		go service.handleBackendMessages(sessionCtx, session, userID, req.ChatID)

		c.JSON(http.StatusOK, StartDeepResearchResponse{
			Success: true,
			Message: "Deep research session started successfully",
		})
	}
}

// DeepResearchHandler handles WebSocket connections for deep research streaming.
func DeepResearchHandler(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage, sessionManager *SessionManager) gin.HandlerFunc {
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

		// Create service instance with shared session manager
		service := NewService(logger, trackingService, firebaseClient, storage, sessionManager)

		// Handle the WebSocket connection
		service.HandleConnection(c.Request.Context(), conn, userID, chatID)
	}
}
