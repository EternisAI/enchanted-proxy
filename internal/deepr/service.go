package deepr

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Service handles WebSocket connections for deep research
type Service struct {
	logger          *logger.Logger
	trackingService *request_tracking.Service
	firebaseClient  *auth.FirebaseClient
	storage         *Storage
	sessionManager  *SessionManager
}

// NewService creates a new deep research service
func NewService(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient) *Service {
	// Get storage path from environment or use default
	storagePath := os.Getenv("DEEPR_STORAGE_PATH")
	if storagePath == "" {
		storagePath = filepath.Join(".", "deepr_sessions")
	}

	storage, err := NewStorage(logger, storagePath)
	if err != nil {
		logger.WithComponent("deepr").Error("failed to create storage, using in-memory only",
			slog.String("error", err.Error()))
	}

	return &Service{
		logger:          logger,
		trackingService: trackingService,
		firebaseClient:  firebaseClient,
		storage:         storage,
		sessionManager:  NewSessionManager(logger),
	}
}

// HandleConnection manages the WebSocket connection and streaming
func (s *Service) HandleConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")
	clientID := uuid.New().String()

	log.Info("handling new client connection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	// Check if this is a reconnection
	isReconnection := s.sessionManager.HasActiveBackend(userID, chatID)

	if isReconnection {
		log.Info("detected reconnection to existing session",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))

		// Handle reconnection
		s.handleReconnection(ctx, clientConn, userID, chatID, clientID)
		return
	}

	// New connection - perform subscription checks
	if err := s.checkAndTrackSubscription(ctx, clientConn, userID); err != nil {
		log.Error("subscription check failed", slog.String("error", err.Error()))
		clientConn.Close()
		return
	}

	// Create new backend connection
	s.handleNewConnection(ctx, clientConn, userID, chatID, clientID)
}

// checkAndTrackSubscription checks user subscription and tracks usage
func (s *Service) checkAndTrackSubscription(ctx context.Context, clientConn *websocket.Conn, userID string) error {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
	if err != nil {
		log.Error("failed to check user subscription status", slog.String("error", err.Error()))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify subscription status"}`))
		return err
	}

	if hasActivePro {
		log.Info("user has active pro subscription",
			slog.String("user_id", userID),
			slog.Time("expires_at", *proExpiresAt))

		if err := s.firebaseClient.IncrementDeepResearchUsage(ctx, userID); err != nil {
			log.Error("failed to track pro user deep research usage", slog.String("error", err.Error()))
		}
	} else {
		log.Info("user is on freemium plan", slog.String("user_id", userID))

		hasUsed, err := s.firebaseClient.HasUsedFreeDeepResearch(ctx, userID)
		if err != nil {
			log.Error("failed to check freemium deep research usage", slog.String("error", err.Error()))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify usage status"}`))
			return err
		}

		if hasUsed {
			log.Info("freemium user has already used their free deep research", slog.String("user_id", userID))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "You have already used your free deep research. Please upgrade to Pro for unlimited access.", "error_code": "FREE_LIMIT_REACHED"}`))
			return err
		}

		if err := s.firebaseClient.MarkFreeDeepResearchUsed(ctx, userID); err != nil {
			log.Error("failed to mark freemium deep research as used", slog.String("error", err.Error()))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to track usage"}`))
			return err
		}

		log.Info("freemium user is using their free deep research", slog.String("user_id", userID))
	}

	return nil
}

// handleReconnection handles a client reconnecting to an existing session
func (s *Service) handleReconnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("handling reconnection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	// Add client to session manager
	s.sessionManager.AddClientConnection(userID, chatID, clientID, clientConn)
	defer s.sessionManager.RemoveClientConnection(userID, chatID, clientID)

	// Check if session is complete
	if s.storage != nil {
		isComplete, err := s.storage.IsSessionComplete(userID, chatID)
		if err != nil {
			log.Error("failed to check session completion status", slog.String("error", err.Error()))
		}

		// Send unsent messages
		unsent, err := s.storage.GetUnsentMessages(userID, chatID)
		if err != nil {
			log.Error("failed to get unsent messages", slog.String("error", err.Error()))
		} else if len(unsent) > 0 {
			log.Info("sending unsent messages to reconnected client",
				slog.Int("count", len(unsent)))

			for _, msg := range unsent {
				if err := clientConn.WriteMessage(websocket.TextMessage, []byte(msg.Message)); err != nil {
					log.Error("failed to send unsent message", slog.String("error", err.Error()))
					return
				}
				// Mark as sent
				if err := s.storage.MarkMessageAsSent(userID, chatID, msg.ID); err != nil {
					log.Error("failed to mark message as sent", slog.String("error", err.Error()))
				}
			}
		}

		if isComplete {
			log.Info("session is complete, no more messages expected")
			return
		}
	}

	// Listen for new messages from backend (they'll be broadcast to all clients)
	done := make(chan struct{})

	// Listen for messages from this client
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			default:
				_, message, err := clientConn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Error("error reading from reconnected client", slog.String("error", err.Error()))
					}
					return
				}

				log.Info("received message from reconnected client", slog.String("message", string(message)))

				// Forward to backend if session exists
				if session, exists := s.sessionManager.GetSession(userID, chatID); exists && session.BackendConn != nil {
					if err := session.BackendConn.WriteMessage(websocket.TextMessage, message); err != nil {
						log.Error("error forwarding message to backend", slog.String("error", err.Error()))
						return
					}
				}
			}
		}
	}()

	<-done
}

// handleNewConnection creates a new backend connection and manages message flow
func (s *Service) handleNewConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	deepResearchHost := os.Getenv("DEEP_RESEARCH_WS")
	if deepResearchHost == "" {
		log.Error("DEEP_RESEARCH_WS environment variable not set")
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Deep research backend not configured"}`))
		return
	}

	wsURL := url.URL{
		Scheme: "ws",
		Host:   deepResearchHost,
		Path:   "/deep_research/" + userID + "/" + chatID + "/",
	}

	log.Info("connecting to deep research server", slog.String("url", wsURL.String()))

	serverConn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		log.Error("failed to connect to deep research server", slog.String("error", err.Error()))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to connect to deep research backend"}`))
		return
	}
	defer serverConn.Close()

	log.Info("connected to deep research backend")

	// Update storage
	if s.storage != nil {
		if err := s.storage.UpdateBackendConnectionStatus(userID, chatID, true); err != nil {
			log.Error("failed to update backend connection status", slog.String("error", err.Error()))
		}
		defer func() {
			if err := s.storage.UpdateBackendConnectionStatus(userID, chatID, false); err != nil {
				log.Error("failed to update backend disconnection status", slog.String("error", err.Error()))
			}
		}()
	}

	// Create session context
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create and register session
	_ = s.sessionManager.CreateSession(userID, chatID, serverConn, sessionCtx, cancel)
	defer s.sessionManager.RemoveSession(userID, chatID)

	// Add initial client
	s.sessionManager.AddClientConnection(userID, chatID, clientID, clientConn)

	done := make(chan struct{})

	// Handle messages from client to backend
	go func() {
		defer close(done)
		for {
			select {
			case <-sessionCtx.Done():
				return
			default:
				_, message, err := clientConn.ReadMessage()
				if err != nil {
					if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Error("error reading from client", slog.String("error", err.Error()))
					}
					s.sessionManager.RemoveClientConnection(userID, chatID, clientID)
					return
				}

				log.Info("received message from client", slog.String("message", string(message)))

				if err := serverConn.WriteMessage(websocket.TextMessage, message); err != nil {
					log.Error("error writing to server", slog.String("error", err.Error()))
					return
				}

				log.Info("message forwarded to deep research backend")
			}
		}
	}()

	// Handle messages from backend to clients
	for {
		select {
		case <-done:
			return
		case <-sessionCtx.Done():
			return
		default:
			_, message, err := serverConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("error reading from backend", slog.String("error", err.Error()))
				}
				return
			}

			log.Info("received message from backend", slog.String("message", string(message)))

			// Determine message type
			var msg Message
			messageType := "status"
			if err := json.Unmarshal(message, &msg); err == nil {
				if msg.Type != "" {
					messageType = msg.Type
				}
			}

			// Store message
			messageSent := false
			if s.storage != nil {
				// Try to broadcast to clients
				broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)
				messageSent = (broadcastErr == nil && s.sessionManager.GetClientCount(userID, chatID) > 0)

				// Store message with sent status
				if err := s.storage.AddMessage(userID, chatID, string(message), messageSent, messageType); err != nil {
					log.Error("failed to store message", slog.String("error", err.Error()))
				} else {
					log.Info("message stored",
						slog.Bool("sent", messageSent),
						slog.String("type", messageType))
				}
			} else {
				// No storage, just broadcast
				s.sessionManager.BroadcastToClients(userID, chatID, message)
			}
		}
	}
}
