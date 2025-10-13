package deepr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"time"

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
	// startTime := time.Now() // DISABLED: Not needed when limit checks are disabled
	log := s.logger.WithContext(ctx).WithComponent("deepr")
	clientID := uuid.New().String()

	log.Info("handling client connection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	// Check if this is a reconnection
	isReconnection := s.sessionManager.HasActiveBackend(userID, chatID)

	if isReconnection {
		log.Info("reconnection to existing session detected",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("client_id", clientID),
			slog.Bool("is_reconnection", true))

		// Handle reconnection
		s.handleReconnection(ctx, clientConn, userID, chatID, clientID)
		return
	}

	log.Info("new session connection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID),
		slog.Bool("is_reconnection", false))

	// New connection - perform subscription checks
	// LIMIT CHECK DISABLED: Allow all users to use deep research
	// if err := s.checkAndTrackSubscription(ctx, clientConn, userID); err != nil {
	// 	log.Error("subscription check failed",
	// 		slog.String("user_id", userID),
	// 		slog.String("chat_id", chatID),
	// 		slog.String("error", err.Error()),
	// 		slog.Duration("duration", time.Since(startTime)))
	// 	clientConn.Close()
	// 	return
	// }

	// Create new backend connection
	s.handleNewConnection(ctx, clientConn, userID, chatID, clientID)
}

// checkAndTrackSubscription checks user subscription and tracks usage
// NOTE: This function is currently DISABLED - all limit checks are commented out
// to allow unrestricted access to deep research for all users
func (s *Service) checkAndTrackSubscription(ctx context.Context, clientConn *websocket.Conn, userID string) error {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("checking user subscription",
		slog.String("user_id", userID))

	hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
	if err != nil {
		log.Error("subscription status check failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
			slog.Duration("duration", time.Since(startTime)))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify subscription status"}`))
		return err
	}

	if hasActivePro {
		// Build log attributes, conditionally adding expires_at if available
		logAttrs := []any{
			slog.String("user_id", userID),
			slog.String("subscription_type", "pro"),
			slog.Duration("check_duration", time.Since(startTime)),
		}
		if proExpiresAt != nil {
			logAttrs = append(logAttrs, slog.Time("expires_at", *proExpiresAt))
		}
		log.Info("pro subscription active", logAttrs...)

		if err := s.firebaseClient.IncrementDeepResearchUsage(ctx, userID); err != nil {
			log.Error("failed to increment usage counter",
				slog.String("user_id", userID),
				slog.String("subscription_type", "pro"),
				slog.String("error", err.Error()))
		} else {
			log.Info("usage tracked successfully",
				slog.String("user_id", userID),
				slog.String("subscription_type", "pro"))
		}
	} else {
		log.Info("freemium subscription detected",
			slog.String("user_id", userID),
			slog.String("subscription_type", "freemium"))

		hasUsed, err := s.firebaseClient.HasUsedFreeDeepResearch(ctx, userID)
		if err != nil {
			log.Error("failed to check freemium usage",
				slog.String("user_id", userID),
				slog.String("subscription_type", "freemium"),
				slog.String("error", err.Error()),
				slog.Duration("duration", time.Since(startTime)))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify usage status"}`))
			return err
		}

		if hasUsed {
			log.Warn("freemium quota exhausted",
				slog.String("user_id", userID),
				slog.String("subscription_type", "freemium"),
				slog.String("error_code", "FREE_LIMIT_REACHED"))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "You have already used your free deep research. Please upgrade to Pro for unlimited access.", "error_code": "FREE_LIMIT_REACHED"}`))
			return fmt.Errorf("freemium quota exhausted for user %s", userID)
		}

		if err := s.firebaseClient.MarkFreeDeepResearchUsed(ctx, userID); err != nil {
			log.Error("failed to mark freemium usage",
				slog.String("user_id", userID),
				slog.String("subscription_type", "freemium"),
				slog.String("error", err.Error()))
			clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to track usage"}`))
			return err
		}

		log.Info("freemium usage marked successfully",
			slog.String("user_id", userID),
			slog.String("subscription_type", "freemium"),
			slog.Duration("duration", time.Since(startTime)))
	}

	return nil
}

// handleReconnection handles a client reconnecting to an existing session
func (s *Service) handleReconnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("reconnection initiated",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	// Check if session is complete and replay unsent messages BEFORE adding client to session manager
	// This prevents concurrent writes: backend broadcast won't know about this client during replay
	if s.storage != nil {
		isComplete, err := s.storage.IsSessionComplete(userID, chatID)
		if err != nil {
			log.Error("failed to check session completion status",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("error", err.Error()))
		}

		// Send unsent messages before registering the connection
		unsent, err := s.storage.GetUnsentMessages(userID, chatID)
		if err != nil {
			log.Error("failed to retrieve unsent messages",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("error", err.Error()))
		} else if len(unsent) > 0 {
			log.Info("replaying unsent messages",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID),
				slog.Int("unsent_count", len(unsent)))

			sentCount := 0
			for _, msg := range unsent {
				if err := clientConn.WriteMessage(websocket.TextMessage, []byte(msg.Message)); err != nil {
					log.Error("failed to replay message",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_id", msg.ID),
						slog.String("error", err.Error()))
					clientConn.Close()
					return
				}
				sentCount++
				// Mark as sent
				if err := s.storage.MarkMessageAsSent(userID, chatID, msg.ID); err != nil {
					log.Error("failed to mark message as sent",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_id", msg.ID),
						slog.String("error", err.Error()))
				}
			}

			log.Info("unsent messages replayed successfully",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.Int("messages_sent", sentCount),
				slog.Duration("replay_duration", time.Since(startTime)))
		} else {
			log.Info("no unsent messages to replay",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID))
		}

		if isComplete {
			log.Info("session already complete, closing connection",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.Duration("duration", time.Since(startTime)))
			clientConn.Close()
			return
		}
	}

	// Now that replay is complete, add client to session manager for future broadcasts
	s.sessionManager.AddClientConnection(userID, chatID, clientID, clientConn)
	defer s.sessionManager.RemoveClientConnection(userID, chatID, clientID)

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
						log.Error("unexpected error reading from reconnected client",
							slog.String("user_id", userID),
							slog.String("chat_id", chatID),
							slog.String("client_id", clientID),
							slog.String("error", err.Error()))
					}
					return
				}

				log.Info("message received from reconnected client",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("client_id", clientID),
					slog.Int("message_size", len(message)))

				// Forward to backend using synchronized write
				if err := s.sessionManager.WriteToBackend(userID, chatID, websocket.TextMessage, message); err != nil {
					log.Error("failed to forward message to backend",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
					return
				}

				log.Debug("message forwarded to backend successfully",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID))
			}
		}
	}()

	<-done
}

// handleClientMessages handles forwarding messages from a client to the backend
func (s *Service) handleClientMessages(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("client message handler started",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	messageCount := 0
	for {
		select {
		case <-ctx.Done():
			log.Info("client message handler stopped - context cancelled",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID),
				slog.Int("messages_processed", messageCount))
			return
		default:
			_, message, err := clientConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("unexpected error reading from client",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("client_id", clientID),
						slog.String("error", err.Error()))
				} else {
					log.Info("client connection closed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("client_id", clientID))
				}
				s.sessionManager.RemoveClientConnection(userID, chatID, clientID)
				return
			}

			messageCount++
			log.Info("message received from client",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("client_id", clientID),
				slog.Int("message_size", len(message)),
				slog.Int("message_number", messageCount))

			// Forward to backend using synchronized write
			if err := s.sessionManager.WriteToBackend(userID, chatID, websocket.TextMessage, message); err != nil {
				log.Error("failed to forward message to backend",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("client_id", clientID),
					slog.String("error", err.Error()))
				return
			}

			log.Debug("message forwarded to backend",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.Int("message_number", messageCount))
		}
	}
}

// handleNewConnection creates a new backend connection and manages message flow
func (s *Service) handleNewConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("initiating new backend connection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	deepResearchHost := os.Getenv("DEEP_RESEARCH_WS")
	if deepResearchHost == "" {
		deepResearchHost = "165.232.133.47:3031"
		log.Info("using default backend host",
			slog.String("host", deepResearchHost),
			slog.String("reason", "DEEP_RESEARCH_WS not set"))
	}

	wsURL := url.URL{
		Scheme: "ws",
		Host:   deepResearchHost,
		Path:   "/deep_research/" + userID + "/" + chatID + "/",
	}

	log.Info("connecting to backend websocket",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("url", wsURL.String()))

	// Create dialer with timeout to prevent indefinite hangs
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 30 * time.Second

	connectStart := time.Now()
	serverConn, _, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		log.Error("backend connection failed",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("url", wsURL.String()),
			slog.String("error", err.Error()),
			slog.Duration("connection_attempt_duration", time.Since(connectStart)))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to connect to deep research backend"}`))
		return
	}
	defer serverConn.Close()

	log.Info("backend connection established",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Duration("connection_time", time.Since(connectStart)))

	// Update storage
	if s.storage != nil {
		if err := s.storage.UpdateBackendConnectionStatus(userID, chatID, true); err != nil {
			log.Error("failed to update backend connection status in storage",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("error", err.Error()))
		}
		defer func() {
			if err := s.storage.UpdateBackendConnectionStatus(userID, chatID, false); err != nil {
				log.Error("failed to update backend disconnection status in storage",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("error", err.Error()))
			}
		}()
	}

	// Create session context derived from incoming context but independent of any single client
	// This ensures cleanup on server shutdown while allowing session to outlive individual clients
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Create and register session
	_ = s.sessionManager.CreateSession(userID, chatID, serverConn, sessionCtx, cancel)
	defer s.sessionManager.RemoveSession(userID, chatID)

	// Add initial client
	s.sessionManager.AddClientConnection(userID, chatID, clientID, clientConn)

	// Handle messages from this client to backend in a separate goroutine
	go s.handleClientMessages(ctx, clientConn, userID, chatID, clientID)

	log.Info("session established, starting message processing",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Duration("setup_duration", time.Since(startTime)))

	// Handle messages from backend to clients - this loop runs until backend disconnects
	messageCount := 0
	for {
		select {
		case <-sessionCtx.Done():
			log.Info("session context cancelled",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.Int("messages_received", messageCount),
				slog.Duration("session_duration", time.Since(startTime)))
			return
		default:
			_, message, err := serverConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("unexpected error reading from backend",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()),
						slog.Int("messages_received", messageCount))
				} else {
					log.Info("backend connection closed normally",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.Int("messages_received", messageCount))
				}
				log.Info("session ending",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.Duration("session_duration", time.Since(startTime)))
				return
			}

			messageCount++
			log.Info("message received from backend",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.Int("message_size", len(message)),
				slog.Int("message_number", messageCount))

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
			clientCount := s.sessionManager.GetClientCount(userID, chatID)

			if s.storage != nil {
				// Try to broadcast to clients
				broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)
				messageSent = (broadcastErr == nil && clientCount > 0)

				// Store message with sent status
				if err := s.storage.AddMessage(userID, chatID, string(message), messageSent, messageType); err != nil {
					log.Error("failed to store message in storage",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType),
						slog.String("error", err.Error()))
				} else {
					log.Debug("message stored successfully",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType),
						slog.Bool("sent", messageSent),
						slog.Int("client_count", clientCount))
				}

				// Track usage only when final_report is sent
				if msg.FinalReport != "" {
					log.Info("final report detected, tracking usage",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType))

					// Check subscription status
					hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
					if err != nil {
						log.Error("failed to check subscription status for usage tracking",
							slog.String("user_id", userID),
							slog.String("chat_id", chatID),
							slog.String("error", err.Error()))
					} else {
						if hasActivePro {
							// Build log attributes, conditionally adding expires_at if available
							logAttrs := []any{
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("subscription_type", "pro"),
							}
							if proExpiresAt != nil {
								logAttrs = append(logAttrs, slog.Time("expires_at", *proExpiresAt))
							}
							log.Info("pro user completed research, incrementing usage counter", logAttrs...)

							if err := s.firebaseClient.IncrementDeepResearchUsage(ctx, userID); err != nil {
								log.Error("failed to increment pro user usage counter",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "pro"),
									slog.String("error", err.Error()))
							} else {
								log.Info("pro user usage tracked successfully",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "pro"))
							}
						} else {
							log.Info("freemium user completed research, marking as used",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("subscription_type", "freemium"))

							if err := s.firebaseClient.MarkFreeDeepResearchUsed(ctx, userID); err != nil {
								log.Error("failed to mark freemium usage",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "freemium"),
									slog.String("error", err.Error()))
							} else {
								log.Info("freemium usage marked successfully",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "freemium"))
							}
						}

						// Save completion data to Firebase
						if err := s.firebaseClient.SaveDeepResearchCompletion(ctx, userID, chatID); err != nil {
							log.Error("failed to save deep research completion to Firebase",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("error", err.Error()))
						} else {
							log.Info("deep research completion saved to Firebase successfully",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID))
						}
					}
				}

				// Check if session is complete
				if msg.FinalReport != "" || msg.Type == "error" || msg.Error != "" || msg.Type == "research_complete" {
					log.Info("session complete - final message received",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType),
						slog.Bool("has_final_report", msg.FinalReport != ""),
						slog.Bool("has_error", msg.Error != ""),
						slog.Bool("is_research_complete", msg.Type == "research_complete"),
						slog.Int("total_messages", messageCount),
						slog.Duration("session_duration", time.Since(startTime)))
					// Final message has been stored and broadcast, now clean up
					// This cancels the session context and exits the loop
					// Defers will close backend connection and remove session from manager
					cancel()
					return
				}
			} else {
				// No storage, just broadcast
				broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)
				if broadcastErr != nil {
					log.Warn("failed to broadcast message without storage",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", broadcastErr.Error()))
				}

				// Track usage only when final_report is sent (even without storage)
				if msg.FinalReport != "" {
					log.Info("final report detected, tracking usage (no storage)",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType))

					// Check subscription status
					hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
					if err != nil {
						log.Error("failed to check subscription status for usage tracking",
							slog.String("user_id", userID),
							slog.String("chat_id", chatID),
							slog.String("error", err.Error()))
					} else {
						if hasActivePro {
							// Build log attributes, conditionally adding expires_at if available
							logAttrs := []any{
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("subscription_type", "pro"),
							}
							if proExpiresAt != nil {
								logAttrs = append(logAttrs, slog.Time("expires_at", *proExpiresAt))
							}
							log.Info("pro user completed research, incrementing usage counter (no storage)", logAttrs...)

							if err := s.firebaseClient.IncrementDeepResearchUsage(ctx, userID); err != nil {
								log.Error("failed to increment pro user usage counter",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "pro"),
									slog.String("error", err.Error()))
							} else {
								log.Info("pro user usage tracked successfully",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "pro"))
							}
						} else {
							log.Info("freemium user completed research, marking as used (no storage)",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("subscription_type", "freemium"))

							if err := s.firebaseClient.MarkFreeDeepResearchUsed(ctx, userID); err != nil {
								log.Error("failed to mark freemium usage",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "freemium"),
									slog.String("error", err.Error()))
							} else {
								log.Info("freemium usage marked successfully",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("subscription_type", "freemium"))
							}
						}

						// Save completion data to Firebase
						if err := s.firebaseClient.SaveDeepResearchCompletion(ctx, userID, chatID); err != nil {
							log.Error("failed to save deep research completion to Firebase",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.String("error", err.Error()))
						} else {
							log.Info("deep research completion saved to Firebase successfully (no storage)",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID))
						}
					}
				}

				// Check if session is complete even without storage
				if msg.FinalReport != "" || msg.Type == "error" || msg.Error != "" || msg.Type == "research_complete" {
					log.Info("session complete - final message received (no storage)",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType),
						slog.Bool("is_research_complete", msg.Type == "research_complete"),
						slog.Int("total_messages", messageCount),
						slog.Duration("session_duration", time.Since(startTime)))
					cancel()
					return
				}
			}
		}
	}
}
