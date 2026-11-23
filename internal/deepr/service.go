package deepr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/notifications"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Service handles WebSocket connections for deep research.
type Service struct {
	logger                       *logger.Logger
	trackingService              *request_tracking.Service
	firebaseClient               *auth.FirebaseClient
	storage                      MessageStorage
	sessionManager               *SessionManager
	encryptionService            *messaging.EncryptionService
	firestoreClient              *messaging.FirestoreClient
	deepResearchRateLimitEnabled bool
	queries                      pgdb.Querier // For tier-based quota enforcement
	notificationService          *notifications.Service
	cfg                          *Config // Limits and backend settings
}

// mapEventTypeToState maps event types from deep research server to session states.
func mapEventTypeToState(eventType string) string {
	switch eventType {
	case "clarification_needed":
		return "clarify"
	case "error":
		return "error"
	case "research_complete":
		return "complete"
	default:
		// All other events (research_progress, etc.) map to in_progress
		return "in_progress"
	}
}

// markRunFailed marks a run failed. It is used when a start fails after the run
// row was created, so the row does not stay active. A fresh context is used so
// the write is not lost to a cancelled request.
func (s *Service) markRunFailed(runID int64) {
	if s.queries == nil || runID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.queries.CompleteDeepResearchRun(ctx, pgdb.CompleteDeepResearchRunParams{
		ID:     runID,
		Status: "failed",
	}); err != nil {
		s.logger.Error("failed to mark deep research run as failed",
			slog.Int64("run_id", runID),
			slog.String("error", err.Error()))
	}
}

// trackDeepResearchTokens records a run's token usage as the backend reports it
// (a running total). The backend enforces the per-run budget; the proxy only records.
func (s *Service) trackDeepResearchTokens(ctx context.Context, runID int64, tokensUsed int) error {
	if s.queries == nil || runID <= 0 || tokensUsed <= 0 {
		return nil
	}
	// Plan tokens keep the multiplier used for deep research so far (3x).
	planTokens := tokensUsed * 3
	if err := s.queries.UpdateDeepResearchRunTokens(ctx, pgdb.UpdateDeepResearchRunTokensParams{
		ID:              runID,
		ModelTokensUsed: int32(tokensUsed),
		PlanTokensUsed:  int32(planTokens),
	}); err != nil {
		return fmt.Errorf("failed to update tokens: %w", err)
	}
	return nil
}

// NewService creates a new deep research service with database storage.
func NewService(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage, sessionManager *SessionManager, queries pgdb.Querier, deepResearchRateLimitEnabled bool, notificationService *notifications.Service, cfg *Config) *Service {
	var encryptionService *messaging.EncryptionService
	var firestoreClient *messaging.FirestoreClient

	if firebaseClient != nil {
		encryptionService = messaging.NewEncryptionService()
		firestoreClient = messaging.NewFirestoreClient(firebaseClient.GetFirestoreClient())
	}

	return &Service{
		logger:                       logger,
		trackingService:              trackingService,
		firebaseClient:               firebaseClient,
		storage:                      storage,
		queries:                      queries,
		sessionManager:               sessionManager,
		encryptionService:            encryptionService,
		firestoreClient:              firestoreClient,
		deepResearchRateLimitEnabled: deepResearchRateLimitEnabled,
		notificationService:          notificationService,
		cfg:                          cfg,
	}
}

// encryptAndStoreMessage handles encryption and Firestore storage for deep research messages.
// It attempts to encrypt the message content with the user's public key, falling back to plaintext if encryption fails.
// Returns the generated message ID and any error encountered.
func (s *Service) encryptAndStoreMessage(ctx context.Context, userID, chatID, content, messageType string, isFromUser bool, customMessageID string) (string, error) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// Check if Firestore client is available
	if s.firestoreClient == nil {
		return "", fmt.Errorf("firestore client not available")
	}

	// Skip if no content to store
	if content == "" {
		log.Warn("no content to store for message",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_type", messageType))
		return "", fmt.Errorf("no content to store")
	}

	// Use custom message ID if provided, otherwise generate a new UUID
	var messageID string
	if customMessageID != "" {
		messageID = customMessageID
	} else {
		messageID = uuid.New().String()
	}

	var encryptedContent string
	var publicKeyStr string

	// Try to encrypt if encryption service is available
	if s.encryptionService != nil {
		// Get user's public key
		publicKey, err := s.firestoreClient.GetUserPublicKey(ctx, userID)
		if err != nil {
			log.Warn("failed to get user public key, saving as plaintext",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("message_id", messageID),
				slog.String("error", err.Error()))
			encryptedContent = content
			publicKeyStr = "none"
		} else {
			// Encrypt the message content
			encrypted, err := s.encryptionService.EncryptMessage(content, publicKey.Public)
			if err != nil {
				log.Warn("failed to encrypt message, saving as plaintext",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_id", messageID),
					slog.String("error", err.Error()))
				encryptedContent = content
				publicKeyStr = "none"
			} else {
				encryptedContent = encrypted
				publicKeyStr = publicKey.Public
			}
		}
	} else {
		// No encryption service available, save as plaintext
		log.Info("encryption service unavailable, saving message as plaintext",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID))
		encryptedContent = content
		publicKeyStr = "none"
	}

	// Create ChatMessage for Firestore
	chatMessage := &messaging.ChatMessage{
		ID:                  messageID,
		EncryptedContent:    encryptedContent,
		IsFromUser:          isFromUser,
		ChatID:              chatID,
		IsError:             messageType == "error",
		Timestamp:           time.Now(),
		PublicEncryptionKey: publicKeyStr,
	}

	// Store message to Firestore at /users/{userID}/chats/{chatID}/messages/{messageID}
	if err := s.firestoreClient.SaveMessage(ctx, userID, chatMessage); err != nil {
		log.Error("failed to save message to Firestore",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("message_id", messageID),
			slog.Bool("encrypted", publicKeyStr != "none"),
			slog.String("error", err.Error()))
		return messageID, err
	}

	log.Info("message saved to Firestore",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.String("message_type", messageType),
		slog.Bool("encrypted", publicKeyStr != "none"))

	return messageID, nil
}

// handleBackendMessages processes messages from the backend websocket for POST API initiated sessions.
func (s *Service) handleBackendMessages(ctx context.Context, session *ActiveSession, userID, chatID string) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")
	startTime := time.Now()
	messageCount := 0
	completedSuccessfully := false

	// Ensure run is marked as completed when function exits
	defer func() {
		if s.queries != nil && session.RunID > 0 {
			// Determine final status
			status := "failed"
			if completedSuccessfully {
				status = "completed"
			}

			// Use fresh context with timeout to ensure DB write succeeds
			completionCtx, cancelCompletion := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelCompletion()

			if err := s.queries.CompleteDeepResearchRun(completionCtx, pgdb.CompleteDeepResearchRunParams{
				ID:     session.RunID,
				Status: status,
			}); err != nil {
				log.Error("failed to mark deep research run as completed in defer (POST /start path)",
					slog.Int64("run_id", session.RunID),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("status", status),
					slog.String("error", err.Error()))
			} else {
				log.Info("deep research run marked as completed in defer (POST /start path)",
					slog.Int64("run_id", session.RunID),
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("status", status))
			}
		}

		s.sessionManager.RemoveSession(userID, chatID)
		if s.storage != nil {
			if err := s.storage.UpdateBackendConnectionStatus(userID, chatID, false); err != nil {
				log.Error("failed to update backend disconnection status",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("error", err.Error()))
			}
		}
		log.Info("backend message handler stopped",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int("total_messages", messageCount),
			slog.Duration("duration", time.Since(startTime)))
	}()

	for {
		select {
		case <-ctx.Done():
			log.Info("context canceled, stopping backend message handler",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID))
			return
		default:
			_, message, err := session.BackendConn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Error("unexpected error reading from backend",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
				} else {
					log.Info("backend connection closed",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID))
				}
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

				// Record token usage if reported by the backend
				if msg.TokensUsed > 0 && session.RunID > 0 {
					if err := s.trackDeepResearchTokens(ctx, session.RunID, msg.TokensUsed); err != nil {
						log.Error("token tracking failed",
							slog.Int64("run_id", session.RunID),
							slog.String("error", err.Error()))
					}
				}
			}

			session.ObserveBackendEvent(messageType)

			// Update session state in Firebase
			sessionState := mapEventTypeToState(messageType)
			if s.firebaseClient != nil {
				if err := s.firebaseClient.UpdateSessionState(ctx, userID, chatID, sessionState); err != nil {
					log.Error("failed to update session state",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
				}

				// Also update chat document state for UI access
				chatState := &auth.DeepResearchState{
					StartedAt: time.Now(), // Will be overwritten on merge if already exists
					Status:    sessionState,
				}

				// Update thinkingState based on message type
				// For progress messages, store the message text as thinking state
				if messageType == "research_progress" && msg.Message != "" {
					chatState.ThinkingState = msg.Message
				} else if messageType == "clarification_needed" || messageType == "research_complete" || messageType == "error" {
					// Clear thinking state for terminal states and clarifications
					chatState.ThinkingState = ""
				}

				// Parse error message if this is an error event
				if messageType == "error" {
					if msg.Error != "" {
						chatState.Error = &auth.DeepResearchError{
							UnderlyingError: msg.Error,
							UserMessage:     "An error occurred during deep research. Please try again.",
						}
					}
				}

				if err := s.firebaseClient.UpdateChatDeepResearchState(ctx, userID, chatID, chatState); err != nil {
					log.Error("failed to update chat deep research state",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
				}
			}

			// Broadcast to connected websocket clients
			clientCount := s.sessionManager.GetClientCount(userID, chatID)
			messageSent := false
			broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)
			if broadcastErr == nil && clientCount > 0 {
				messageSent = true
			}

			// Store message in database
			if s.storage != nil {
				if err := s.storage.AddMessage(userID, chatID, string(message), messageSent, messageType); err != nil {
					log.Error("failed to store message",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
				}
			}

			// Store message to Firestore at /users/{userID}/chats/{chatID}/messages/{messageID}
			// Only store clarifications and final reports as messages (not progress updates)
			if s.firestoreClient != nil &&
				(messageType == "clarification_needed" || messageType == "research_complete") {
				// Extract the actual content from the message
				// Python backend sends content in the "message" field
				contentToStore := msg.Message

				// Use helper method to encrypt and store message (no custom ID for assistant messages)
				_, _ = s.encryptAndStoreMessage(ctx, userID, chatID, contentToStore, messageType, false, "")
			}

			// Check if session is complete
			if msg.Type == "research_complete" || msg.Type == "error" || msg.Error != "" {
				log.Info("research session complete",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_type", messageType))

				// Mark as successful if research completed without error
				if msg.Type == "research_complete" {
					completedSuccessfully = true

					// Send push notification for successful completion
					if s.notificationService != nil {
						go func() {
							// Use background context to ensure notification sends even if session context is cancelled
							notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()

							if err := s.notificationService.SendDeepResearchCompletionNotification(notifyCtx, userID, chatID); err != nil {
								log.Error("failed to send deep research completion notification",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("error", err.Error()))
							}
						}()
					}
				}

				return
			}
		}
	}
}

// HandleConnection manages the WebSocket connection and streaming.
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

	log.Info("reconnection check performed",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID),
		slog.Bool("has_active_backend", isReconnection))

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

	// A new connection starts a run: apply the same checks as every other start path.
	refuse := func(message string) {
		if errJSON, err := json.Marshal(map[string]string{"error": message}); err == nil {
			clientConn.WriteMessage(websocket.TextMessage, errJSON)
		}
		clientConn.Close()
	}
	tierConfig, _, err := s.trackingService.GetUserTierConfig(ctx, userID)
	if err != nil {
		log.Error("failed to get user tier", slog.String("error", err.Error()))
		refuse("Failed to validate access")
		return
	}
	var identity *auth.Identity
	if id, ok := auth.IdentityFromContext(ctx); ok {
		identity = &id
	}
	runID, fe, err := s.startRun(ctx, userID, chatID, identity, tierConfig)
	if fe != nil {
		refuse(fe.Error)
		return
	}
	if err != nil {
		log.Error("failed to create run record", slog.String("error", err.Error()))
		refuse("Failed to initialize session")
		return
	}

	// Create new backend connection
	s.handleNewConnection(ctx, clientConn, userID, chatID, clientID, runID)
}

// checkAndTrackSubscription checks user subscription and tracks usage
// NOTE: This function is currently DISABLED - all limit checks are commented out
// to allow unrestricted access to deep research for all users.
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

// handleReconnection handles a client reconnecting to an existing session.
func (s *Service) handleReconnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	log.Info("reconnection initiated",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	// A reconnection joins the caller's own running session (sessions are keyed by
	// user and chat) and starts no run, so the run limits are not checked again.

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

	// Get the session to access its context
	session, exists := s.sessionManager.GetSession(userID, chatID)
	if !exists || session == nil {
		log.Error("session not found after reconnection",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		clientConn.Close()
		return
	}

	// Listen for messages from this client
	// Use session context so client can disconnect without terminating the backend session
	go func() {
		defer close(done)
		for {
			select {
			case <-session.Context.Done():
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

				// Forward only what the run accepts: the query, then a reply to each
				// clarification question, at most twice. Anything else would start more
				// work in the backend within the same counted run.
				// Claim and write on the session this client joined, never on a
				// later run that took the same chat.
				allowed, reason := session.ClaimClientInput()
				if !allowed {
					log.Warn("message from client not forwarded", slog.String("reason", reason))
					if errJSON, err := json.Marshal(map[string]string{"error": reason}); err == nil {
						clientConn.WriteMessage(websocket.TextMessage, errJSON)
					}
					continue
				}

				// Forward to backend using synchronized write
				if err := session.WriteToBackend(websocket.TextMessage, message); err != nil {
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

// handleClientMessages forwards messages from a client to the backend of session,
// the run the client started.
func (s *Service) handleClientMessages(sessionCtx context.Context, session *ActiveSession, clientConn *websocket.Conn, userID, chatID, clientID string) {
	log := s.logger.WithContext(sessionCtx).WithComponent("deepr")

	log.Info("client message handler started",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	messageCount := 0
	for {
		select {
		case <-sessionCtx.Done():
			log.Info("client message handler stopped - session ended",
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

			// Forward only what the run accepts: the query, then a reply to each
			// clarification question, at most twice. Anything else would start more
			// work in the backend within the same counted run.
			allowed, reason := session.ClaimClientInput()
			if !allowed {
				log.Warn("message from client not forwarded", slog.String("reason", reason))
				if errJSON, err := json.Marshal(map[string]string{"error": reason}); err == nil {
					clientConn.WriteMessage(websocket.TextMessage, errJSON)
				}
				continue
			}

			// Forward to backend using synchronized write
			if err := session.WriteToBackend(websocket.TextMessage, message); err != nil {
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

// handleNewConnection creates a new backend connection and manages message flow.
func (s *Service) handleNewConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string, runID int64) {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// The run was recorded at admission. Mark it completed or failed whenever this
	// returns, including when the backend cannot be reached. Registered first, so
	// it runs after the session is removed. Uses a fresh context, not ctx.
	completedSuccessfully := false
	defer func() {
		if s.queries == nil || runID <= 0 {
			return
		}

		// Determine final status
		status := "failed"
		if completedSuccessfully {
			status = "completed"
		}

		// Use fresh context with timeout to ensure DB write succeeds
		completionCtx, cancelCompletion := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCompletion()

		if err := s.queries.CompleteDeepResearchRun(completionCtx, pgdb.CompleteDeepResearchRunParams{
			ID:     runID,
			Status: status,
		}); err != nil {
			log.Error("failed to mark deep research run as completed in defer",
				slog.Int64("run_id", runID),
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("status", status),
				slog.String("error", err.Error()))
		} else {
			log.Info("deep research run marked as completed in defer",
				slog.Int64("run_id", runID),
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("status", status))
		}
	}()

	log.Info("initiating new backend connection",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("client_id", clientID))

	deepResearchHost := os.Getenv("DEEP_RESEARCH_WS")
	if deepResearchHost == "" {
		deepResearchHost = "localhost:3031"
		log.Info("using default backend host",
			slog.String("host", deepResearchHost),
			slog.String("reason", "DEEP_RESEARCH_WS not set"))
	}

	deepResearchScheme := os.Getenv("DEEP_RESEARCH_WS_SCHEME")
	if deepResearchScheme == "" {
		deepResearchScheme = "ws"
	}

	wsURL := backendSessionURL(deepResearchScheme, deepResearchHost, userID, chatID)

	log.Info("connecting to backend websocket",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("url", wsURL.String()))

	// Create dialer with timeout to prevent indefinite hangs
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 30 * time.Second

	connectStart := time.Now()
	serverConn, _, err := dialer.Dial(wsURL.String(), s.cfg.backendHeaders())
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

	// Create session context independent of any single client's request context
	// This allows the backend connection to outlive individual client disconnections
	// while still allowing cleanup when the session completes
	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create and register session with runID for token tracking
	session := s.sessionManager.CreateSession(userID, chatID, runID, serverConn, sessionCtx, cancel, queryFromClient)
	defer s.sessionManager.RemoveSession(userID, chatID)

	// Check if user has premium to log parallel session creation
	hasActivePro, _, _ := s.trackingService.HasActivePro(ctx, userID)
	if hasActivePro {
		log.Info("PREMIUM user - new deep research session created (parallel sessions enabled)",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("client_id", clientID))
	}

	// Add initial client
	s.sessionManager.AddClientConnection(userID, chatID, clientID, clientConn)

	// Handle messages from this client to backend in a separate goroutine
	// Use session context so client can disconnect without terminating the session
	go s.handleClientMessages(sessionCtx, session, clientConn, userID, chatID, clientID)

	log.Info("session established, starting message processing",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Duration("setup_duration", time.Since(startTime)))

	// Handle messages from backend to clients - this loop runs until backend disconnects
	messageCount := 0
	for {
		select {
		case <-sessionCtx.Done():
			log.Info("session context canceled",
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
				// Record token usage if reported by the backend
				if msg.TokensUsed > 0 {
					if err := s.trackDeepResearchTokens(ctx, runID, msg.TokensUsed); err != nil {
						log.Error("token tracking failed", slog.Int64("run_id", runID), slog.String("error", err.Error()))
					}
				}
			}
			// This reader's own run, even if a later start took the chat's key.
			session.ObserveBackendEvent(messageType)

			// Update session state in Firebase based on message type
			sessionState := mapEventTypeToState(messageType)
			if err := s.firebaseClient.UpdateSessionState(ctx, userID, chatID, sessionState); err != nil {
				log.Error("failed to update session state in Firebase",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_type", messageType),
					slog.String("session_state", sessionState),
					slog.String("error", err.Error()))
			} else {
				log.Debug("session state updated in Firebase",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_type", messageType),
					slog.String("session_state", sessionState))
			}

			// Also update chat document state for UI access
			chatState := &auth.DeepResearchState{
				StartedAt: time.Now(), // Will be overwritten on merge if already exists
				Status:    sessionState,
			}

			// Update thinkingState based on message type
			// For progress messages, store the message text as thinking state
			if messageType == "research_progress" && msg.Content != "" {
				chatState.ThinkingState = msg.Content
			} else if messageType == "clarification_needed" || messageType == "research_complete" || messageType == "error" {
				// Clear thinking state for terminal states and clarifications
				chatState.ThinkingState = ""
			}

			// Parse error message if this is an error event
			if messageType == "error" {
				if msg.Error != "" {
					chatState.Error = &auth.DeepResearchError{
						UnderlyingError: msg.Error,
						UserMessage:     "An error occurred during deep research. Please try again.",
					}
				}
			}

			if err := s.firebaseClient.UpdateChatDeepResearchState(ctx, userID, chatID, chatState); err != nil {
				log.Error("failed to update chat deep research state",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("error", err.Error()))
			}

			// Store message
			messageSent := false
			clientCount := s.sessionManager.GetClientCount(userID, chatID)

			if s.storage != nil {
				// Try to broadcast to clients
				broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)
				messageSent = (broadcastErr == nil && clientCount > 0)

				// Log detailed message info for debugging
				log.Info("broadcasting message to clients",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_type", messageType),
					slog.Bool("is_complete", msg.Type == "research_complete"),
					slog.Int("client_count", clientCount),
					slog.Bool("broadcast_success", broadcastErr == nil))

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

				// Store message to Firestore at /users/{userID}/chats/{chatID}/messages/{messageID}
				// Only store clarifications and final reports as messages (not progress updates)
				if s.firestoreClient != nil &&
					(messageType == "clarification_needed" || messageType == "research_complete") {
					// Extract the actual content from the message
					// Python backend sends content in the "message" field
					contentToStore := msg.Message

					// Use helper method to encrypt and store message (no custom ID for assistant messages)
					_, _ = s.encryptAndStoreMessage(ctx, userID, chatID, contentToStore, messageType, false, "")
				}

				// Track usage only when research_complete event is sent
				if msg.Type == "research_complete" {
					log.Info("research complete event detected, tracking usage",
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

					// Mark as successful for defer completion
					completedSuccessfully = true

					// Send push notification for successful completion
					if s.notificationService != nil {
						go func() {
							// Use background context to ensure notification sends even if session context is cancelled
							notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()

							if err := s.notificationService.SendDeepResearchCompletionNotification(notifyCtx, userID, chatID); err != nil {
								log.Error("failed to send deep research completion notification",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("error", err.Error()))
							}
						}()
					}
				}

				// Check if session is complete
				if msg.Type == "research_complete" || msg.Type == "error" || msg.Error != "" {
					log.Info("session complete - final message received",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("message_type", messageType),
						slog.Bool("is_complete", msg.Type == "research_complete"),
						slog.Bool("has_error", msg.Error != ""),
						slog.Bool("is_research_complete", msg.Type == "research_complete"),
						slog.Int("total_messages", messageCount),
						slog.Duration("session_duration", time.Since(startTime)))

					// Final message has been stored and broadcast, now clean up
					// This cancels the session context and exits the loop
					// Defers will close backend connection, mark run as completed, and remove session from manager
					cancel()
					return
				}
			} else {
				// No storage, just broadcast
				broadcastErr := s.sessionManager.BroadcastToClients(userID, chatID, message)

				// Log detailed message info for debugging (no storage)
				log.Info("broadcasting message to clients (no storage)",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("message_type", messageType),
					slog.Bool("is_complete", msg.Type == "research_complete"),
					slog.Bool("broadcast_success", broadcastErr == nil))
				if broadcastErr != nil {
					log.Warn("failed to broadcast message without storage",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", broadcastErr.Error()))
				}

				// Store message to Firestore at /users/{userID}/chats/{chatID}/messages/{messageID} (even without storage)
				if s.firestoreClient != nil &&
					(messageType == "clarification_needed" || messageType == "research_complete") {
					// Extract the actual content from the message
					// Python backend sends content in the "message" field
					contentToStore := msg.Message

					// Use helper method to encrypt and store message (no custom ID for assistant messages)
					_, _ = s.encryptAndStoreMessage(ctx, userID, chatID, contentToStore, messageType, false, "")
				}

				// Track usage only when research_complete event is sent (even without storage)
				if msg.Type == "research_complete" {
					log.Info("research complete event detected, tracking usage (no storage)",
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

					// Mark as successful for defer completion
					completedSuccessfully = true

					// Send push notification for successful completion
					if s.notificationService != nil {
						go func() {
							// Use background context to ensure notification sends even if session context is cancelled
							notifyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()

							if err := s.notificationService.SendDeepResearchCompletionNotification(notifyCtx, userID, chatID); err != nil {
								log.Error("failed to send deep research completion notification",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.String("error", err.Error()))
							}
						}()
					}
				}

				// Check if session is complete even without storage
				if msg.Type == "research_complete" || msg.Type == "error" || msg.Error != "" {
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
