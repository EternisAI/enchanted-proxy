package deepr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
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

// canForwardMessage checks if a message from the client should be forwarded to the backend
// based on the current session state. Messages can only be forwarded when state is 'clarify' or 'error'.
func (s *Service) canForwardMessage(ctx context.Context, userID, chatID string) (bool, string, error) {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	sessionState, err := s.firebaseClient.GetSessionState(ctx, userID, chatID)
	if err != nil {
		log.Error("failed to get session state for message forwarding check",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return false, "", fmt.Errorf("failed to get session state: %w", err)
	}

	// If no session state exists yet, allow forwarding (initial message)
	if sessionState == nil {
		log.Debug("no session state found, allowing initial message",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		return true, "", nil
	}

	// Only allow message forwarding when state is 'clarify' or 'error'
	canForward := sessionState.State == "clarify" || sessionState.State == "error"

	log.Info("message forwarding check",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("session_state", sessionState.State),
		slog.Bool("can_forward", canForward))

	if !canForward {
		return false, sessionState.State, nil
	}

	return true, sessionState.State, nil
}

// validateFreemiumAccess checks if a user can start or continue a deep research session
// Premium users (HasActivePro = true): Can have UNLIMITED parallel deep research sessions with no restrictions
// Freemium users: Limited to 1 active session at a time and 1 total completed session lifetime
// Returns nil if user is allowed to proceed, error otherwise.
func (s *Service) validateFreemiumAccess(ctx context.Context, userID, chatID string, isReconnection bool) error {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// Skip validation if deep research rate limiting is disabled
	if !s.deepResearchRateLimitEnabled {
		log.Info("deep research rate limiting disabled, skipping freemium validation",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		return nil
	}

	// Check if user has active pro subscription
	hasActivePro, proExpiresAt, err := s.trackingService.HasActivePro(ctx, userID)
	if err != nil {
		log.Error("failed to check subscription status",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to verify subscription status")
	}

	// Premium users can have UNLIMITED parallel sessions - bypass all restrictions
	if hasActivePro {
		expiryInfo := "unlimited"
		if proExpiresAt != nil {
			expiryInfo = proExpiresAt.Format("2006-01-02 15:04:05 MST")
		}
		log.Info("PREMIUM user detected - allowing parallel deep research sessions (no limits)",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("pro_expires_at", expiryInfo),
			slog.Bool("is_reconnection", isReconnection))
		// Immediately return success - premium users bypass ALL freemium restrictions
		return nil
	}

	// Freemium user - check restrictions
	log.Info("freemium user detected, checking access restrictions",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.Bool("is_reconnection", isReconnection))

	// Get current session state
	sessionState, err := s.firebaseClient.GetSessionState(ctx, userID, chatID)
	if err != nil {
		log.Error("failed to get session state",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to verify session state")
	}

	// If this is a reconnection or existing session
	if sessionState != nil {
		log.Info("existing session found",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("session_state", sessionState.State))

		// Allow reconnection/continuation if state is 'clarify' or 'in_progress'
		if sessionState.State == "clarify" || sessionState.State == "in_progress" {
			log.Info("freemium user allowed to continue existing session",
				slog.String("user_id", userID),
				slog.String("chat_id", chatID),
				slog.String("session_state", sessionState.State))
			return nil
		}

		// If state is 'complete' or 'error', check if user has other completed sessions
		if sessionState.State == "complete" || sessionState.State == "error" {
			completedCount, err := s.firebaseClient.GetCompletedSessionCountForUser(ctx, userID)
			if err != nil {
				log.Error("failed to get completed session count",
					slog.String("user_id", userID),
					slog.String("error", err.Error()))
				return fmt.Errorf("failed to verify usage status")
			}

			if completedCount >= 1 {
				log.Warn("freemium quota exhausted - user has completed session",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.Int("completed_count", completedCount))
				return fmt.Errorf("you have already used your free deep research. Please upgrade to Pro for unlimited access")
			}
		}

		return nil
	}

	// New session - check if user already has completed research
	completedCount, err := s.firebaseClient.GetCompletedSessionCountForUser(ctx, userID)
	if err != nil {
		log.Error("failed to get completed session count",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to verify usage status")
	}

	if completedCount >= 1 {
		log.Warn("freemium quota exhausted - user already has completed research",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int("completed_count", completedCount))
		return fmt.Errorf("you have already used your free deep research. Please upgrade to Pro for unlimited access")
	}

	// Check if user has any active (in_progress or clarify) sessions
	activeSessions, err := s.firebaseClient.GetActiveSessionsForUser(ctx, userID)
	if err != nil {
		log.Error("failed to get active sessions",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to verify active sessions")
	}

	if len(activeSessions) > 0 {
		log.Warn("FREEMIUM user blocked - already has an active session (premium users would have bypassed this check)",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.Int("active_sessions_count", len(activeSessions)),
			slog.Bool("has_active_pro", false)) // This should always be false here
		return fmt.Errorf("you already have an active deep research session. Please complete or cancel it before starting a new one. Upgrade to Pro for unlimited parallel sessions")
	}

	log.Info("freemium user allowed to start new session",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID))
	return nil
}

// NewService creates a new deep research service with database storage.
func NewService(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage, sessionManager *SessionManager, deepResearchRateLimitEnabled bool) *Service {
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
		sessionManager:               sessionManager,
		encryptionService:            encryptionService,
		firestoreClient:              firestoreClient,
		deepResearchRateLimitEnabled: deepResearchRateLimitEnabled,
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

	defer func() {
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
			}

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

	// Validate freemium access for new connections
	if err := s.validateFreemiumAccess(ctx, userID, chatID, false); err != nil {
		log.Error("freemium validation failed",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		// Send error message to client
		errorMsg := map[string]string{
			"error": err.Error(),
		}
		if errJSON, marshalErr := json.Marshal(errorMsg); marshalErr == nil {
			clientConn.WriteMessage(websocket.TextMessage, errJSON)
		}
		clientConn.Close()
		return
	}

	// Create new backend connection
	s.handleNewConnection(ctx, clientConn, userID, chatID, clientID)
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

	// Validate freemium access for reconnections
	if err := s.validateFreemiumAccess(ctx, userID, chatID, true); err != nil {
		log.Error("freemium validation failed for reconnection",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		// Send error message to client
		errorMsg := map[string]string{
			"error": err.Error(),
		}
		if errJSON, marshalErr := json.Marshal(errorMsg); marshalErr == nil {
			clientConn.WriteMessage(websocket.TextMessage, errJSON)
		}
		clientConn.Close()
		return
	}

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

				// Check if message can be forwarded based on session state
				canForward, currentState, err := s.canForwardMessage(ctx, userID, chatID)
				if err != nil {
					log.Error("failed to check if message can be forwarded",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("error", err.Error()))
					// Send error to client
					clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify session state"}`))
					continue
				}

				if !canForward {
					log.Warn("message blocked - session state does not allow user input",
						slog.String("user_id", userID),
						slog.String("chat_id", chatID),
						slog.String("session_state", currentState))
					// Send error to client indicating they cannot send messages in current state
					errMsg := fmt.Sprintf(`{"error": "Cannot send messages while research is in progress", "session_state": "%s"}`, currentState)
					clientConn.WriteMessage(websocket.TextMessage, []byte(errMsg))
					continue
				}

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

// handleClientMessages handles forwarding messages from a client to the backend.
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
			log.Info("client message handler stopped - context canceled",
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

			// Check if message can be forwarded based on session state
			canForward, currentState, err := s.canForwardMessage(ctx, userID, chatID)
			if err != nil {
				log.Error("failed to check if message can be forwarded",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("error", err.Error()))
				// Send error to client
				clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to verify session state"}`))
				continue
			}

			if !canForward {
				log.Warn("message blocked - session state does not allow user input",
					slog.String("user_id", userID),
					slog.String("chat_id", chatID),
					slog.String("session_state", currentState))
				// Send error to client indicating they cannot send messages in current state
				errMsg := fmt.Sprintf(`{"error": "Cannot send messages while research is in progress", "session_state": "%s"}`, currentState)
				clientConn.WriteMessage(websocket.TextMessage, []byte(errMsg))
				continue
			}

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

// handleNewConnection creates a new backend connection and manages message flow.
func (s *Service) handleNewConnection(ctx context.Context, clientConn *websocket.Conn, userID, chatID, clientID string) {
	startTime := time.Now()
	log := s.logger.WithContext(ctx).WithComponent("deepr")

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

	wsURL := url.URL{
		Scheme: deepResearchScheme,
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

	// Create session context independent of any single client's request context
	// This allows the backend connection to outlive individual client disconnections
	// while still allowing cleanup when the session completes
	sessionCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create and register session
	_ = s.sessionManager.CreateSession(userID, chatID, serverConn, sessionCtx, cancel)
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
			}

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
					// Defers will close backend connection and remove session from manager
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
