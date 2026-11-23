package deepr

import (
	"context"
	stderrors "errors"
	"log/slog"
	"sync"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/gorilla/websocket"
)

// ActiveSession represents an active backend connection.
type ActiveSession struct {
	UserID         string
	ChatID         string
	RunID          int64 // Database run ID for token tracking
	BackendConn    *websocket.Conn
	Context        context.Context
	CancelFunc     context.CancelFunc
	mu             sync.RWMutex               // Protects clientConns map
	backendWriteMu sync.Mutex                 // Serializes writes to backend websocket
	clientConns    map[string]*websocket.Conn // Map of client connection IDs

	// What the client may send to the backend: the query, then one reply after
	// each clarification question, at most maxClarificationRounds times. Each
	// further message would start more work in the backend within the same run.
	inputMu        sync.Mutex
	querySent      bool
	awaitingReply  bool
	clarifications int
}

// maxClarificationRounds is how many clarification replies one run accepts.
const maxClarificationRounds = 2

// ClaimClientInput reports whether a message from the client may be forwarded to
// the backend now and, if so, records it. The reason explains a refusal.
func (s *ActiveSession) ClaimClientInput() (bool, string) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	switch {
	case !s.querySent:
		s.querySent = true
		return true, ""
	case !s.awaitingReply:
		return false, "Cannot send messages while research is in progress"
	case s.clarifications >= maxClarificationRounds:
		return false, "This research has already had its clarification rounds"
	default:
		s.awaitingReply = false
		s.clarifications++
		return true, ""
	}
}

// WriteToBackend sends a message on this session's backend connection. Writes
// are serialized: the connection supports one writer at a time.
func (s *ActiveSession) WriteToBackend(messageType int, data []byte) error {
	s.backendWriteMu.Lock()
	defer s.backendWriteMu.Unlock()
	if s.BackendConn == nil {
		return errBackendClosed
	}
	return s.BackendConn.WriteMessage(messageType, data)
}

var errBackendClosed = stderrors.New("backend connection closed")

// querySource says who sends a new session's query to the backend.
type querySource int

const (
	// queryFromClient: the client's first message is the query (WebSocket start).
	queryFromClient querySource = iota
	// queryFromProxy: the proxy sends the query itself (REST start), so no
	// client message may take its place.
	queryFromProxy
)

// ObserveBackendEvent updates what the client may send after a backend message.
func (s *ActiveSession) ObserveBackendEvent(eventType string) {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	switch eventType {
	case "clarification_needed":
		s.awaitingReply = true
	case "error", "research_complete":
		s.awaitingReply = false
	}
}

// SessionManager manages active backend connections.
type SessionManager struct {
	logger   *logger.Logger
	sessions map[string]*ActiveSession // key: "userID:chatID"
	mu       sync.RWMutex
}

// NewSessionManager creates a new session manager.
func NewSessionManager(logger *logger.Logger) *SessionManager {
	return &SessionManager{
		logger:   logger,
		sessions: make(map[string]*ActiveSession),
	}
}

// getSessionKey generates a session key from userID and chatID.
func (sm *SessionManager) getSessionKey(userID, chatID string) string {
	return userID + ":" + chatID
}

// GetSession retrieves an active session.
func (sm *SessionManager) GetSession(userID, chatID string) (*ActiveSession, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	key := sm.getSessionKey(userID, chatID)
	session, exists := sm.sessions[key]
	return session, exists
}

// CreateSession creates a new active session. With queryFromProxy, the session
// is published with its query already claimed.
func (sm *SessionManager) CreateSession(userID, chatID string, runID int64, backendConn *websocket.Conn, ctx context.Context, cancel context.CancelFunc, query querySource) *ActiveSession {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := sm.getSessionKey(userID, chatID)

	// Check if session already exists
	if existingSession, exists := sm.sessions[key]; exists {
		// Safely read client count
		existingSession.mu.RLock()
		existingClientCount := len(existingSession.clientConns)
		existingSession.mu.RUnlock()

		sm.logger.WithComponent("deepr-session").Warn("OVERWRITING existing session",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("session_key", key),
			slog.Int("existing_client_count", existingClientCount))

		// Proactively cancel and close sockets to avoid leaks
		if existingSession.CancelFunc != nil {
			existingSession.CancelFunc()
		}
		if existingSession.BackendConn != nil {
			_ = existingSession.BackendConn.Close()
		}
		existingSession.mu.Lock()
		for _, c := range existingSession.clientConns {
			_ = c.Close()
		}
		existingSession.clientConns = make(map[string]*websocket.Conn)
		existingSession.mu.Unlock()
	}

	session := &ActiveSession{
		UserID:      userID,
		ChatID:      chatID,
		RunID:       runID,
		BackendConn: backendConn,
		Context:     ctx,
		CancelFunc:  cancel,
		clientConns: make(map[string]*websocket.Conn),
		querySent:   query == queryFromProxy,
	}

	sm.sessions[key] = session

	sm.logger.WithComponent("deepr-session").Info("session created",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Int64("run_id", runID),
		slog.String("session_key", key),
		slog.Int("total_active_sessions", len(sm.sessions)))

	return session
}

// RemoveSession removes a session.
func (sm *SessionManager) RemoveSession(userID, chatID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	key := sm.getSessionKey(userID, chatID)

	sm.logger.WithComponent("deepr-session").Info("RemoveSession called",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("session_key", key))

	if session, exists := sm.sessions[key]; exists {
		// Close all client connections
		session.mu.Lock()
		clientCount := len(session.clientConns)
		for clientID, conn := range session.clientConns {
			conn.Close()
			sm.logger.WithComponent("deepr-session").Debug("client connection closed during cleanup",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID))
		}
		session.clientConns = make(map[string]*websocket.Conn)
		session.mu.Unlock()

		// Cancel context
		if session.CancelFunc != nil {
			session.CancelFunc()
		}

		delete(sm.sessions, key)

		sm.logger.WithComponent("deepr-session").Info("session removed",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("session_key", key),
			slog.Int("closed_clients", clientCount),
			slog.Int("remaining_active_sessions", len(sm.sessions)))
	}
}

// AddClientConnection adds a client connection to an existing session.
func (sm *SessionManager) AddClientConnection(userID, chatID, clientID string, conn *websocket.Conn) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	key := sm.getSessionKey(userID, chatID)
	if session, exists := sm.sessions[key]; exists {
		session.mu.Lock()
		session.clientConns[clientID] = conn
		totalClients := len(session.clientConns)
		session.mu.Unlock()

		sm.logger.WithComponent("deepr-session").Info("client connection added",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("client_id", clientID),
			slog.Int("total_clients", totalClients))
	} else {
		sm.logger.WithComponent("deepr-session").Warn("attempted to add client to non-existent session",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("client_id", clientID))
	}
}

// RemoveClientConnection removes a client connection from a session.
func (sm *SessionManager) RemoveClientConnection(userID, chatID, clientID string) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	key := sm.getSessionKey(userID, chatID)
	if session, exists := sm.sessions[key]; exists {
		session.mu.Lock()
		_, wasPresent := session.clientConns[clientID]
		delete(session.clientConns, clientID)
		clientCount := len(session.clientConns)
		session.mu.Unlock()

		if wasPresent {
			sm.logger.WithComponent("deepr-session").Info("client connection removed",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID),
				slog.Int("remaining_clients", clientCount))
		} else {
			sm.logger.WithComponent("deepr-session").Debug("attempted to remove non-existent client connection",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID))
		}
	}
}

// BroadcastToClients sends a message to all connected clients for a session.
func (sm *SessionManager) BroadcastToClients(userID, chatID string, message []byte) error {
	sm.mu.RLock()
	key := sm.getSessionKey(userID, chatID)
	session, exists := sm.sessions[key]
	sm.mu.RUnlock()

	if !exists {
		sm.logger.WithComponent("deepr-session").Debug("no active session for broadcast",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		return nil // No active session, message will be stored as unsent
	}

	session.mu.RLock()
	defer session.mu.RUnlock()

	var lastErr error
	sentCount := 0
	failedCount := 0
	totalClients := len(session.clientConns)

	for clientID, conn := range session.clientConns {
		if err := conn.WriteMessage(websocket.TextMessage, message); err != nil {
			sm.logger.WithComponent("deepr-session").Error("failed to broadcast to client",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID),
				slog.String("error", err.Error()))
			lastErr = err
			failedCount++
		} else {
			sentCount++
		}
	}

	if totalClients > 0 {
		sm.logger.WithComponent("deepr-session").Debug("broadcast completed",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int("message_size", len(message)),
			slog.Int("sent_count", sentCount),
			slog.Int("failed_count", failedCount),
			slog.Int("total_clients", totalClients))
	}

	return lastErr
}

// GetClientCount returns the number of connected clients for a session.
func (sm *SessionManager) GetClientCount(userID, chatID string) int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	key := sm.getSessionKey(userID, chatID)
	if session, exists := sm.sessions[key]; exists {
		session.mu.RLock()
		defer session.mu.RUnlock()
		return len(session.clientConns)
	}

	return 0
}

// HasActiveBackend checks if there's an active backend connection for a session.
func (sm *SessionManager) HasActiveBackend(userID, chatID string) bool {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	key := sm.getSessionKey(userID, chatID)
	session, exists := sm.sessions[key]

	var clientCount int
	if exists {
		session.mu.RLock()
		clientCount = len(session.clientConns)
		session.mu.RUnlock()
	}

	sm.logger.WithComponent("deepr-session").Debug("HasActiveBackend called",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("session_key", key),
		slog.Bool("session_exists", exists),
		slog.Int("client_count", clientCount),
		slog.Int("total_sessions", len(sm.sessions)))

	return exists
}
