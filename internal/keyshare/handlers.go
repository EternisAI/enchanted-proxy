package keyshare

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for now
	},
}

// Handler handles HTTP requests for key sharing
type Handler struct {
	service          *Service
	websocketManager *WebSocketManager
	logger           *logger.Logger
}

// NewHandler creates a new key sharing handler
func NewHandler(service *Service, websocketManager *WebSocketManager, logger *logger.Logger) *Handler {
	return &Handler{
		service:          service,
		websocketManager: websocketManager,
		logger:           logger,
	}
}

// CreateSession handles POST /api/v1/encryption/key-share/session
func (h *Handler) CreateSession(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("keyshare_handler")

	// Get user ID from auth context
	userID, exists := auth.GetUserID(c)
	if !exists {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error:   "invalid_token",
			Message: "Firebase authentication failed",
		})
		return
	}

	// Parse request body
	var req CreateSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("invalid request body",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "validation_error",
			Message: err.Error(),
		})
		return
	}

	// Create session
	resp, err := h.service.CreateSession(c.Request.Context(), userID, req)
	if err != nil {
		statusCode := http.StatusInternalServerError
		errorCode := "internal_error"
		message := "Failed to create session"

		switch status.Code(err) {
		case codes.InvalidArgument:
			statusCode = http.StatusBadRequest
			errorCode = "validation_error"
			message = err.Error()
		case codes.ResourceExhausted:
			statusCode = http.StatusTooManyRequests
			errorCode = "rate_limit_exceeded"
			message = err.Error()
		}

		c.JSON(statusCode, ErrorResponse{
			Error:   errorCode,
			Message: message,
		})
		return
	}

	c.JSON(http.StatusOK, resp)
}

// SubmitKey handles POST /api/v1/encryption/key-share/session/:sessionId
func (h *Handler) SubmitKey(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("keyshare_handler")

	// Get user ID from auth context
	userID, exists := auth.GetUserID(c)
	if !exists {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error:   "invalid_token",
			Message: "Firebase authentication failed",
		})
		return
	}

	// Get session ID from URL
	sessionID := c.Param("sessionId")
	if sessionID == "" {
		log.Error("session ID missing",
			slog.String("user_id", userID))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "validation_error",
			Message: "sessionId parameter is required",
		})
		return
	}

	// Parse request body
	var req SubmitKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		log.Error("invalid request body",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "validation_error",
			Message: err.Error(),
		})
		return
	}

	// Submit encrypted key
	err := h.service.SubmitEncryptedKey(c.Request.Context(), userID, sessionID, req)
	if err != nil {
		statusCode := http.StatusInternalServerError
		errorCode := "internal_error"
		message := "Failed to process key submission"

		switch status.Code(err) {
		case codes.NotFound:
			statusCode = http.StatusNotFound
			errorCode = "session_not_found"
			message = "Session not found or expired"
		case codes.PermissionDenied:
			errors.AbortWithForbidden(c, errors.SessionNotOwned(sessionID))
			return
		case codes.FailedPrecondition:
			statusCode = http.StatusConflict
			errorCode = "session_completed"
			message = err.Error()
		case codes.DeadlineExceeded:
			statusCode = http.StatusNotFound
			errorCode = "session_expired"
			message = "Session expired"
		}

		c.JSON(statusCode, ErrorResponse{
			Error:   errorCode,
			Message: message,
		})
		return
	}

	c.JSON(http.StatusOK, SubmitKeyResponse{Success: true})
}

// WebSocketListen handles WebSocket GET /api/v1/encryption/key-share/session/:sessionId/listen
func (h *Handler) WebSocketListen(c *gin.Context) {
	log := h.logger.WithContext(c.Request.Context()).WithComponent("keyshare_websocket")

	// Get user ID from auth context
	userID, exists := auth.GetUserID(c)
	if !exists {
		log.Error("user not authenticated")
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error:   "invalid_token",
			Message: "Firebase authentication failed",
		})
		return
	}

	// Get session ID from URL
	sessionID := c.Param("sessionId")
	if sessionID == "" {
		log.Error("session ID missing",
			slog.String("user_id", userID))
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error:   "validation_error",
			Message: "sessionId parameter is required",
		})
		return
	}

	// Validate session ownership before upgrading connection
	session, err := h.service.GetSession(c.Request.Context(), sessionID)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Warn("session not found",
				slog.String("user_id", userID),
				slog.String("session_id", sessionID))
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error:   "session_not_found",
				Message: "Session not found",
			})
			return
		}
		log.Error("failed to get session",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error:   "internal_error",
			Message: "Failed to validate session",
		})
		return
	}

	if session.UserID != userID {
		log.Warn("session ownership validation failed",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("session_owner", session.UserID))
		errors.AbortWithForbidden(c, errors.SessionNotOwned(sessionID))
		return
	}

	// Check concurrent connection limit
	if h.websocketManager.GetUserConnectionCount(userID) >= MaxConcurrentWebSocketsPerUser {
		log.Warn("too many concurrent connections",
			slog.String("user_id", userID),
			slog.Int("count", h.websocketManager.GetUserConnectionCount(userID)))
		c.JSON(http.StatusTooManyRequests, ErrorResponse{
			Error:   "too_many_connections",
			Message: "Maximum concurrent WebSocket connections exceeded",
		})
		return
	}

	// Upgrade to WebSocket
	log.Info("upgrading connection to websocket",
		slog.String("user_id", userID),
		slog.String("session_id", sessionID))

	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Error("websocket upgrade failed",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return
	}
	defer conn.Close()

	log.Info("websocket connection established",
		slog.String("user_id", userID),
		slog.String("session_id", sessionID))

	// Register connection
	h.websocketManager.RegisterConnection(sessionID, userID, conn)
	defer h.websocketManager.UnregisterConnection(conn)

	// Send connected message
	connectedMsg := WebSocketMessage{
		Type:      WSMessageTypeConnected,
		SessionID: sessionID,
	}
	if err := conn.WriteJSON(connectedMsg); err != nil {
		log.Error("failed to send connected message",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return
	}

	// Setup ping/pong for keep-alive (30 seconds)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Setup session expiration timeout
	expirationDuration := time.Until(session.ExpiresAt)
	if expirationDuration < 0 {
		expirationDuration = 0
	}
	expirationTimer := time.NewTimer(expirationDuration)
	defer expirationTimer.Stop()

	// Handle pong messages
	conn.SetPongHandler(func(string) error {
		log.Debug("pong received",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID))
		return nil
	})

	// Keep connection alive until key is received or session expires
	done := make(chan struct{})
	defer close(done)

	// Read messages (mostly for detecting disconnection)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				log.Info("connection closed by client",
					slog.String("user_id", userID),
					slog.String("session_id", sessionID))
				close(done)
				return
			}
		}
	}()

	for {
		select {
		case <-ticker.C:
			// Send ping
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Error("failed to send ping",
					slog.String("user_id", userID),
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()))
				return
			}

		case <-expirationTimer.C:
			// Session expired
			log.Info("session expired",
				slog.String("user_id", userID),
				slog.String("session_id", sessionID))

			expiredMsg := WebSocketMessage{
				Type:    WSMessageTypeSessionExpired,
				Message: "Session expired after 5 minutes",
			}
			conn.WriteJSON(expiredMsg)
			conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Session expired"))
			return

		case <-done:
			// Connection closed
			return
		}
	}
}
