package deepr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/errors"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
	"github.com/eternisai/enchanted-proxy/internal/request_tracking"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
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

// checkDeepResearchQuota validates run limits and enforces per-run caps using tier-based system.
// This replaces the old Firestore-based freemium validation with PostgreSQL tier system.
// Returns *errors.ForbiddenError for quota violations, or a regular error for system failures.
func (s *Service) checkDeepResearchQuota(ctx context.Context, userID string, tierConfig tiers.Config) *errors.ForbiddenError {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	if !s.deepResearchRateLimitEnabled {
		log.Info("deep research rate limiting disabled, skipping quota check",
			slog.String("user_id", userID))
		return nil
	}

	// Check active jobs (only applies when MaxActiveSessions == 1, i.e., Free tier)
	// Pro tier has MaxActiveSessions == 0 (unlimited), so this check is skipped
	if tierConfig.DeepResearchMaxActiveSessions == 1 {
		hasActive, err := s.queries.HasActiveDeepResearchRun(ctx, userID)
		if err != nil {
			log.Error("failed to check active runs",
				slog.String("user_id", userID),
				slog.String("error", err.Error()))
			return errors.TierValidationFailed("failed to check active runs")
		}
		if hasActive {
			log.Warn("user blocked - already has active deep research run",
				slog.String("user_id", userID),
				slog.Int("max_active", tierConfig.DeepResearchMaxActiveSessions))
			return errors.ActiveDeepResearchSession(tierConfig.Name, tierConfig.DisplayName, tierConfig.DeepResearchMaxActiveSessions)
		}
	}

	// Check daily runs (Pro tier: 10 runs/day)
	if tierConfig.DeepResearchDailyRuns > 0 {
		count, err := s.queries.GetUserDeepResearchRunsToday(ctx, userID)
		if err != nil {
			log.Error("failed to check daily runs",
				slog.String("user_id", userID),
				slog.String("error", err.Error()))
			return errors.TierValidationFailed("failed to check daily runs")
		}
		if int(count) >= tierConfig.DeepResearchDailyRuns {
			log.Warn("daily deep research limit exceeded",
				slog.String("user_id", userID),
				slog.Int64("runs_today", count),
				slog.Int("daily_limit", tierConfig.DeepResearchDailyRuns))
			now := time.Now().UTC()
			nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			return errors.DeepResearchDailyLimit(tierConfig.Name, tierConfig.DisplayName, count, int64(tierConfig.DeepResearchDailyRuns), nextMidnight)
		}
	}

	// Check lifetime runs (Free tier: 1 run total)
	if tierConfig.DeepResearchLifetimeRuns > 0 {
		count, err := s.queries.GetUserDeepResearchRunsLifetime(ctx, userID)
		if err != nil {
			log.Error("failed to check lifetime runs",
				slog.String("user_id", userID),
				slog.String("error", err.Error()))
			return errors.TierValidationFailed("failed to check lifetime runs")
		}
		if int(count) >= tierConfig.DeepResearchLifetimeRuns {
			log.Warn("lifetime deep research limit reached",
				slog.String("user_id", userID),
				slog.Int64("lifetime_runs", count),
				slog.Int("lifetime_limit", tierConfig.DeepResearchLifetimeRuns))
			return errors.DeepResearchLifetimeLimit(tierConfig.Name, tierConfig.DisplayName, count, int64(tierConfig.DeepResearchLifetimeRuns))
		}
	}

	log.Info("deep research quota check passed",
		slog.String("user_id", userID),
		slog.String("tier", tierConfig.Name))
	return nil
}

// trackDeepResearchTokens updates run token usage and enforces per-run cap.
func (s *Service) trackDeepResearchTokens(
	ctx context.Context,
	runID int64,
	tokensUsed int,
	tierConfig tiers.Config,
) error {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// Calculate plan tokens (GLM-4.6 multiplier = 3×)
	planTokens := tokensUsed * 3

	// Check per-run cap (cap is in raw GLM-4.6 tokens: 8k for free, 10k for pro)
	cap := tierConfig.DeepResearchTokenCap

	if tokensUsed > cap {
		log.Warn("deep research run exceeded token cap",
			slog.Int64("run_id", runID),
			slog.Int("tokens_used", tokensUsed),
			slog.Int("plan_tokens", planTokens),
			slog.Int("cap", cap))

		// Terminate run
		if err := s.queries.CompleteDeepResearchRun(ctx, pgdb.CompleteDeepResearchRunParams{
			ID:     runID,
			Status: "failed",
		}); err != nil {
			log.Error("failed to terminate run after cap exceeded",
				slog.Int64("run_id", runID),
				slog.String("error", err.Error()))
			return fmt.Errorf("failed to terminate run: %w", err)
		}

		return fmt.Errorf("per-run token limit exceeded (%d/%d raw tokens)", tokensUsed, cap)
	}

	// Update token usage
	if err := s.queries.UpdateDeepResearchRunTokens(ctx, pgdb.UpdateDeepResearchRunTokensParams{
		ID:              runID,
		ModelTokensUsed: int32(tokensUsed),
		PlanTokensUsed:  int32(planTokens),
	}); err != nil {
		log.Error("failed to update run tokens",
			slog.Int64("run_id", runID),
			slog.Int("tokens", tokensUsed),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to update tokens: %w", err)
	}

	log.Debug("updated deep research run tokens",
		slog.Int64("run_id", runID),
		slog.Int("tokens_used", tokensUsed),
		slog.Int("plan_tokens", planTokens),
		slog.Int("cap", cap))

	return nil
}

// validateFreemiumAccess is DEPRECATED - kept for backwards compatibility during migration.
// Use checkDeepResearchQuota instead.
func (s *Service) validateFreemiumAccess(ctx context.Context, userID, chatID string, isReconnection bool) error {
	log := s.logger.WithContext(ctx).WithComponent("deepr")

	// Skip validation if deep research rate limiting is disabled
	if !s.deepResearchRateLimitEnabled {
		log.Info("deep research rate limiting disabled, skipping freemium validation",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID))
		return nil
	}

	// Get user's tier configuration
	tierConfig, _, err := s.trackingService.GetUserTierConfig(ctx, userID)
	if err != nil {
		log.Error("failed to get user tier",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return fmt.Errorf("failed to verify subscription status")
	}

	// Delegate to new tier-based quota check
	if forbiddenErr := s.checkDeepResearchQuota(ctx, userID, tierConfig); forbiddenErr != nil {
		// Convert ForbiddenError to regular error for backwards compatibility
		return fmt.Errorf("%s", forbiddenErr.UIMessage)
	}
	return nil
}

// NewService creates a new deep research service with database storage.
func NewService(logger *logger.Logger, trackingService *request_tracking.Service, firebaseClient *auth.FirebaseClient, storage MessageStorage, sessionManager *SessionManager, queries pgdb.Querier, deepResearchRateLimitEnabled bool) *Service {
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

				// Track token usage if reported by backend
				if msg.TokensUsed > 0 && session.RunID > 0 {
					// Get user's tier config for token cap enforcement
					tierConfig, _, err := s.trackingService.GetUserTierConfig(ctx, userID)
					if err != nil {
						log.Error("failed to get user tier for token tracking",
							slog.String("user_id", userID),
							slog.Int64("run_id", session.RunID),
							slog.String("error", err.Error()))
					} else {
						// Track tokens with multiplier and cap enforcement
						if err := s.trackDeepResearchTokens(ctx, session.RunID, msg.TokensUsed, tierConfig); err != nil {
							log.Error("token tracking failed",
								slog.String("user_id", userID),
								slog.String("chat_id", chatID),
								slog.Int64("run_id", session.RunID),
								slog.Int("tokens_used", msg.TokensUsed),
								slog.String("error", err.Error()))

							// If token cap exceeded, this is a terminal error - close session
							if strings.Contains(err.Error(), "token limit exceeded") {
								log.Warn("closing session due to token cap",
									slog.String("user_id", userID),
									slog.String("chat_id", chatID),
									slog.Int64("run_id", session.RunID))
								return
							}
						} else {
							log.Debug("tracked deep research tokens",
								slog.String("user_id", userID),
								slog.Int64("run_id", session.RunID),
								slog.Int("tokens_used", msg.TokensUsed))
						}
					}
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

				// Mark as successful if research completed without error
				if msg.Type == "research_complete" {
					completedSuccessfully = true
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

	// Create run record for token tracking
	runID, err := s.queries.CreateDeepResearchRun(ctx, pgdb.CreateDeepResearchRunParams{
		UserID: userID,
		ChatID: chatID,
	})
	if err != nil {
		log.Error("failed to create run record",
			slog.String("user_id", userID),
			slog.String("chat_id", chatID),
			slog.String("error", err.Error()))
		clientConn.WriteMessage(websocket.TextMessage, []byte(`{"error": "Failed to initialize session"}`))
		return
	}

	// Ensure run is marked as completed when function exits (regardless of how it exits)
	// Use background context to avoid cancellation issues
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

	// Create and register session with runID for token tracking
	_ = s.sessionManager.CreateSession(userID, chatID, runID, serverConn, sessionCtx, cancel)
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

					// Mark as successful for defer completion
					completedSuccessfully = true
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
