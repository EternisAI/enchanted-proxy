package keyshare

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gorilla/websocket"
)

// WebSocketManager manages WebSocket connections for key sharing sessions
type WebSocketManager struct {
	// connections maps sessionID -> set of WebSocket connections
	connections map[string]map[*websocket.Conn]bool

	// userConnections maps userID -> set of WebSocket connections (for rate limiting)
	userConnections map[string]map[*websocket.Conn]bool

	// connToSession maps WebSocket connection -> sessionID (for cleanup)
	connToSession map[*websocket.Conn]string

	// connToUser maps WebSocket connection -> userID (for cleanup)
	connToUser map[*websocket.Conn]string

	mu     sync.RWMutex
	logger *logger.Logger
}

// NewWebSocketManager creates a new WebSocket manager
func NewWebSocketManager(logger *logger.Logger) *WebSocketManager {
	return &WebSocketManager{
		connections:     make(map[string]map[*websocket.Conn]bool),
		userConnections: make(map[string]map[*websocket.Conn]bool),
		connToSession:   make(map[*websocket.Conn]string),
		connToUser:      make(map[*websocket.Conn]string),
		logger:          logger,
	}
}

// RegisterConnection registers a new WebSocket connection for a session
func (m *WebSocketManager) RegisterConnection(sessionID, userID string, conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Register by session
	if m.connections[sessionID] == nil {
		m.connections[sessionID] = make(map[*websocket.Conn]bool)
	}
	m.connections[sessionID][conn] = true

	// Register by user (for rate limiting)
	if m.userConnections[userID] == nil {
		m.userConnections[userID] = make(map[*websocket.Conn]bool)
	}
	m.userConnections[userID][conn] = true

	// Track reverse mappings for cleanup
	m.connToSession[conn] = sessionID
	m.connToUser[conn] = userID

	m.logger.WithComponent("websocket_manager").Debug("connection registered",
		slog.String("session_id", sessionID),
		slog.String("user_id", userID),
		slog.Int("session_connections", len(m.connections[sessionID])),
		slog.Int("user_connections", len(m.userConnections[userID])))
}

// UnregisterConnection removes a WebSocket connection
func (m *WebSocketManager) UnregisterConnection(conn *websocket.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sessionID := m.connToSession[conn]
	userID := m.connToUser[conn]

	// Unregister by session
	if sessionConns, ok := m.connections[sessionID]; ok {
		delete(sessionConns, conn)
		if len(sessionConns) == 0 {
			delete(m.connections, sessionID)
		}
	}

	// Unregister by user
	if userConns, ok := m.userConnections[userID]; ok {
		delete(userConns, conn)
		if len(userConns) == 0 {
			delete(m.userConnections, userID)
		}
	}

	// Cleanup reverse mappings
	delete(m.connToSession, conn)
	delete(m.connToUser, conn)

	m.logger.WithComponent("websocket_manager").Debug("connection unregistered",
		slog.String("session_id", sessionID),
		slog.String("user_id", userID))
}

// SendToSession sends a message to all connections listening to a session
func (m *WebSocketManager) SendToSession(sessionID string, message WebSocketMessage) error {
	m.mu.RLock()
	sessionConns, ok := m.connections[sessionID]
	if !ok || len(sessionConns) == 0 {
		m.mu.RUnlock()
		m.logger.WithComponent("websocket_manager").Warn("no connections found for session",
			slog.String("session_id", sessionID))
		return nil
	}

	// Create a copy of connections to avoid holding lock during send
	conns := make([]*websocket.Conn, 0, len(sessionConns))
	for conn := range sessionConns {
		conns = append(conns, conn)
	}
	m.mu.RUnlock()

	messageBytes, err := json.Marshal(message)
	if err != nil {
		m.logger.WithComponent("websocket_manager").Error("failed to marshal message",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return err
	}

	log := m.logger.WithComponent("websocket_manager")
	for _, conn := range conns {
		if conn == nil {
			continue
		}

		// Send message
		err := conn.WriteMessage(websocket.TextMessage, messageBytes)
		if err != nil {
			log.Error("failed to send message",
				slog.String("session_id", sessionID),
				slog.String("error", err.Error()))
			continue
		}

		log.Info("message sent to connection",
			slog.String("session_id", sessionID),
			slog.String("message_type", message.Type))

		// Close connection after successful delivery (one-time use)
		if message.Type == WSMessageTypeKeyReceived {
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Key delivered successfully"))
			conn.Close()
			m.UnregisterConnection(conn)
		}
	}

	return nil
}

// GetUserConnectionCount returns the number of active connections for a user
func (m *WebSocketManager) GetUserConnectionCount(userID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if userConns, ok := m.userConnections[userID]; ok {
		return len(userConns)
	}
	return 0
}

// GetSessionConnectionCount returns the number of active connections for a session
func (m *WebSocketManager) GetSessionConnectionCount(sessionID string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if sessionConns, ok := m.connections[sessionID]; ok {
		return len(sessionConns)
	}
	return 0
}
