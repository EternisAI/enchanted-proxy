package deepr

import (
	"context"
	"log/slog"
	"net/url"
	"os"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/gorilla/websocket"
)

// Service handles WebSocket connections for deep research
type Service struct {
	logger          *logger.Logger
	trackingService *request_tracking.Service
	firebaseClient  *auth.FirebaseClient
}

// NewService creates a new deep research service
func NewService(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient) *Service {
	return &Service{
		logger:          logger,
		trackingService: trackingService,
		firebaseClient:  firebaseClient,
	}
}

// HandleConnection manages the WebSocket connection and streaming
func (s *Service) HandleConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// Check user subscription status
	hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
	if err != nil {
		log.Error("failed to check user subscription status", slog.String("error", err.Error()))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify subscription status"}`))
		clientConn.Close()
		return
	}

	if hasActivePro {
		log.Info("user has active pro subscription",
			slog.String("user_id", userID),
			slog.Time("expires_at", *proExpiresAt))

		// Track usage for pro users (for analytics)
		if err := s.firebaseClient.IncrementDeepResearchUsage(ctx, userID); err != nil {
			log.Error("failed to track pro user deep research usage", slog.String("error", err.Error()))
			// Don't block pro users on tracking error
		}
	} else {
		// Freemium user - check if they've already used their free deep research
		log.Info("user is on freemium plan", slog.String("user_id", userID))

		hasUsed, err := s.firebaseClient.HasUsedFreeDeepResearch(ctx, userID)
		if err != nil {
			log.Error("failed to check freemium deep research usage", slog.String("error", err.Error()))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify usage status"}`))
			clientConn.Close()
			return
		}

		if hasUsed {
			log.Info("freemium user has already used their free deep research", slog.String("user_id", userID))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "You have already used your free deep research. Please upgrade to Pro for unlimited access.", "error_code": "FREE_LIMIT_REACHED"}`))
			clientConn.Close()
			return
		}

		// Mark that the freemium user has now used their free deep research
		if err := s.firebaseClient.MarkFreeDeepResearchUsed(ctx, userID); err != nil {
			log.Error("failed to mark freemium deep research as used", slog.String("error", err.Error()))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to track usage"}`))
			clientConn.Close()
			return
		}

		log.Info("freemium user is using their free deep research", slog.String("user_id", userID))
	}

	// Construct WebSocket URL for the deep research server
	deepResearchHost := os.Getenv("DEEP_RESEARCH_WS")
	if deepResearchHost == "" {
		log.Error("❌ [DeepResearch] DEEP_RESEARCH_WS environment variable not set")
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Deep research backend not configured"}`))
		return
	}

	wsURL := url.URL{
		Scheme: "ws",
		Host:   deepResearchHost,
		Path:   "/deep_research/" + userID + "/" + chatID + "/",
	}

	log.Info("🔌 [DeepResearch] connecting to deep research server", slog.String("url", wsURL.String()))

	// Connect to the deep research server
	serverConn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		log.Error("❌ [DeepResearch] failed to connect to deep research server", slog.String("error", err.Error()))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to connect to deep research backend"}`))
		return
	}
	defer serverConn.Close()

	log.Info("✅ [DeepResearch] Connected to deep research backend")

	// Create channels for communication
	done := make(chan struct{})

	// Start goroutine to handle messages from client to server
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
						log.Error("❌ [DeepResearch] error reading from client", slog.String("error", err.Error()))
					}
					return
				}

				log.Info("📨 [DeepResearch] Received message from client", slog.String("message", string(message)))

				// Forward message to server
				if err := serverConn.WriteMessage(websocket.TextMessage, message); err != nil {
					log.Error("❌ [DeepResearch] error writing to server", slog.String("error", err.Error()))
					return
				}

				log.Info("📤 [DeepResearch] message forwarded to deep research backend")
			}
		}
	}()

	// Handle messages from server to client
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
			_, message, err := serverConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("❌ [DeepResearch] error reading from backend", slog.String("error", err.Error()))
				}
				return
			}

			log.Info("📨 [DeepResearch] Received message from backend", slog.String("message", string(message)))

			// Forward message to client
			if err := clientConn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Error("❌ [DeepResearch] error writing to client", slog.String("error", err.Error()))
				return
			}

			log.Info("📤 [DeepResearch] message forwarded to client")
		}
	}
}
