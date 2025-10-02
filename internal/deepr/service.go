package deepr

import (
	"context"
	"log/slog"
	"net/url"
	"os"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	"github.com/gorilla/websocket"
)

// Service handles WebSocket connections for deep research
type Service struct {
	logger          *logger.Logger
	trackingService *request_tracking.Service
}

// NewService creates a new deep research service
func NewService(logger *logger.Logger, trackingService *request_tracking.Service) *Service {
	return &Service{
		logger:          logger,
		trackingService: trackingService,
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
	} else {
		log.Info("user is on freemium plan", slog.String("user_id", userID))
	}

	// TODO: Implement subscription-based access control
	// For now, allow both pro and freemium users
	// In the future, you might want to:
	// - Limit freemium users to certain features
	// - Rate limit freemium users
	// - Block freemium users from certain endpoints

	// Construct WebSocket URL for the deep research server
	wsURL := url.URL{
		Scheme: "ws",
		Host:   os.Getenv("DEEP_RESEARCH_WS"),
		Path:   "/deep_research/" + userID + "/" + chatID + "/",
	}

	log.Info("connecting to deep research server", slog.String("url", wsURL.String()))

	// Connect to the deep research server
	serverConn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
	if err != nil {
		log.Error("failed to connect to deep research server", slog.String("error", err.Error()))
		return
	}
	defer serverConn.Close()

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
						log.Error("error reading from client", slog.String("error", err.Error()))
					}
					return
				}

				// Forward message to server
				if err := serverConn.WriteMessage(websocket.TextMessage, message); err != nil {
					log.Error("error writing to server", slog.String("error", err.Error()))
					return
				}

				log.Debug("message forwarded to server", slog.String("message", string(message)))
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
					log.Error("error reading from server", slog.String("error", err.Error()))
				}
				return
			}

			// Forward message to client
			if err := clientConn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Error("error writing to client", slog.String("error", err.Error()))
				return
			}

			log.Debug("message forwarded to client", slog.String("message", string(message)))
		}
	}
}
