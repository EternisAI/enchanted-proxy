package messaging

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/google/uuid"
)

// Service handles async message storage with encryption
type Service struct {
	firestoreClient   *FirestoreClient
	encryptionService *EncryptionService
	logger            *logger.Logger
	messageChan       chan MessageToStore
	workerPool        sync.WaitGroup
	shutdown          chan struct{}
	closed            atomic.Bool
}

// NewService creates a new message storage service
func NewService(firestoreClient *firestore.Client, logger *logger.Logger) *Service {
	s := &Service{
		firestoreClient:   NewFirestoreClient(firestoreClient),
		encryptionService: NewEncryptionService(),
		logger:            logger,
		messageChan:       make(chan MessageToStore, config.AppConfig.MessageStorageBufferSize), // Buffered channel to queue messages waiting for workers
		shutdown:          make(chan struct{}),
	}

	// Start worker pool - each worker processes messages concurrently from the queue
	for i := 0; i < config.AppConfig.MessageStorageWorkerPoolSize; i++ {
		s.workerPool.Add(1)
		go s.worker()
	}

	logger.Info("message storage service started",
		slog.Int("worker_pool_size", config.AppConfig.MessageStorageWorkerPoolSize),
		slog.Int("buffer_size", config.AppConfig.MessageStorageBufferSize),
	)

	return s
}

// worker processes messages from the channel
func (s *Service) worker() {
	defer s.workerPool.Done()

	for {
		select {
		case msg := <-s.messageChan:
			s.handleMessage(msg)
		case <-s.shutdown:
			// Drain remaining messages
			for {
				select {
				case msg := <-s.messageChan:
					s.handleMessage(msg)
				default:
					return
				}
			}
		}
	}
}

// handleMessage processes and stores a single message
func (s *Service) handleMessage(msg MessageToStore) {
	// Timeout context prevents workers from hanging on slow/failed Firestore operations
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.AppConfig.MessageStorageTimeoutSeconds)*time.Second)
	defer cancel()

	log := s.logger.WithContext(ctx)

	// Generate message ID if not provided
	if msg.MessageID == "" {
		msg.MessageID = uuid.New().String()
	}

	// Handle encryption based on client's explicit X-Encryption-Enabled header
	var encryptedContent string
	var publicKeyUsed string

	// Case 1: Client explicitly requests encryption (encryptionEnabled = true)
	if msg.EncryptionEnabled != nil && *msg.EncryptionEnabled {
		log.Debug("encryption explicitly enabled by client",
			slog.String("user_id", msg.UserID))

		publicKey, err := s.getPublicKey(ctx, msg.UserID)
		if err != nil {
			// STRICT MODE: If client expects encryption, we MUST encrypt
			log.Error("encryption enabled but no public key found",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID),
				slog.String("message_id", msg.MessageID),
				slog.String("error", err.Error()))
			return // Fail: don't store if client expects encryption
		}

		encrypted, err := s.encryptionService.EncryptMessage(msg.Content, publicKey.Public)
		if err != nil {
			log.Error("encryption failed (client expects encryption)",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID),
				slog.String("message_id", msg.MessageID),
				slog.String("error", err.Error()))
			return // Fail: don't store if encryption fails
		}

		encryptedContent = encrypted
		publicKeyUsed = publicKey.Public
		log.Info("message encrypted per client request",
			slog.String("user_id", msg.UserID),
			slog.String("message_id", msg.MessageID))
	} else if msg.EncryptionEnabled != nil && !*msg.EncryptionEnabled {
		// Case 2: Client explicitly disables encryption (encryptionEnabled = false)
		log.Info("encryption explicitly disabled by client, storing plaintext",
			slog.String("user_id", msg.UserID),
			slog.String("message_id", msg.MessageID))

		encryptedContent = msg.Content
		publicKeyUsed = "none"
	} else {
		// Case 3: Backward compatibility - header not provided (encryptionEnabled = nil)
		// Use existing graceful degradation logic
		log.Debug("encryption header not provided, using backward compatible logic",
			slog.String("user_id", msg.UserID))

		publicKey, err := s.getPublicKey(ctx, msg.UserID)
		if err != nil {
			// GRACEFUL DEGRADATION: Store as plaintext if no public key available
			// This allows gradual rollout - users without encryption setup still get message storage
			// Set MESSAGE_STORAGE_REQUIRE_ENCRYPTION=true for strict E2EE mode (rejects storage on encryption failure)
			if config.AppConfig.MessageStorageRequireEncryption {
				log.Error("cannot store message without encryption (strict mode enabled)",
					slog.String("user_id", msg.UserID),
					slog.String("error", err.Error()))
				return // Fail-safe: refuse to store
			}

			log.Warn("failed to get public key, storing unencrypted",
				slog.String("user_id", msg.UserID),
				slog.String("error", err.Error()))
			encryptedContent = msg.Content
			publicKeyUsed = "none"
		} else {
			encrypted, err := s.encryptionService.EncryptMessage(msg.Content, publicKey.Public)
			if err != nil {
				// GRACEFUL DEGRADATION: Store as plaintext if encryption fails
				if config.AppConfig.MessageStorageRequireEncryption {
					log.Error("encryption failed, refusing to store (strict mode enabled)",
						slog.String("user_id", msg.UserID),
						slog.String("error", err.Error()))
					return // Fail-safe: refuse to store
				}

				log.Error("encryption failed, storing unencrypted",
					slog.String("user_id", msg.UserID),
					slog.String("error", err.Error()))
				encryptedContent = msg.Content
				publicKeyUsed = "none"
			} else {
				encryptedContent = encrypted
				publicKeyUsed = publicKey.Public // Store the full JWK
			}
		}
	}

	// Create Firestore message
	chatMsg := &ChatMessage{
		ID:                  msg.MessageID,
		EncryptedContent:    encryptedContent,
		IsFromUser:          msg.IsFromUser,
		ChatID:              msg.ChatID,
		IsError:             msg.IsError,
		Timestamp:           time.Now(),
		PublicEncryptionKey: publicKeyUsed,
		Stopped:             msg.Stopped,
		StoppedBy:           msg.StoppedBy,
		StopReason:          msg.StopReason,
		Model:               msg.Model,
		GenerationState:     msg.GenerationState,
		GenerationError:     msg.GenerationError,
	}

	// Set generation timestamps if provided
	if msg.GenerationStartedAt != nil {
		chatMsg.GenerationStartedAt = *msg.GenerationStartedAt
	}
	if msg.GenerationCompletedAt != nil {
		chatMsg.GenerationCompletedAt = *msg.GenerationCompletedAt
	}

	// Save to Firestore
	if err := s.firestoreClient.SaveMessage(ctx, msg.UserID, chatMsg); err != nil {
		log.Error("failed to save message to firestore",
			slog.String("user_id", msg.UserID),
			slog.String("chat_id", msg.ChatID),
			slog.String("message_id", msg.MessageID),
			slog.String("error", err.Error()))
		return
	}

	log.Debug("message saved successfully",
		slog.String("user_id", msg.UserID),
		slog.String("chat_id", msg.ChatID),
		slog.String("message_id", msg.MessageID),
		slog.Bool("encrypted", publicKeyUsed != "none"))
}

// getPublicKey retrieves public key from Firestore (no caching - simpler and always fresh)
func (s *Service) getPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	log := s.logger.WithContext(ctx)

	// Fetch from Firestore
	key, err := s.firestoreClient.GetUserPublicKey(ctx, userID)
	if err != nil {
		log.Error("failed to fetch public key from Firestore",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return nil, err
	}

	// Check if key is valid
	if key == nil || key.Public == "" {
		log.Error("public key is empty",
			slog.String("user_id", userID),
		)
		return nil, fmt.Errorf("no public key found for user %s", userID)
	}

	// Validate key
	if err := s.encryptionService.ValidatePublicKey(key.Public); err != nil {
		log.Error("public key validation failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return nil, fmt.Errorf("invalid public key: %w", err)
	}

	log.Debug("public key fetched successfully",
		slog.String("user_id", userID),
	)

	return key, nil
}

// StoreMessageAsync queues a message for async storage
func (s *Service) StoreMessageAsync(ctx context.Context, msg MessageToStore) error {
	if s.closed.Load() {
		return fmt.Errorf("service is shutting down")
	}

	// Wait up to 5 seconds for queue space (no silent drops)
	select {
	case s.messageChan <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		// Queue blocked for 5 seconds - this indicates a serious problem
		// Check once more if shutting down before blocking
		if s.closed.Load() {
			s.logger.Warn("service is shutting down, cannot queue after timeout",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID))
			return fmt.Errorf("service is shutting down")
		}

		// Log as error and try one more time with limited timeout
		s.logger.Error("message queue blocked for 5s, attempting blocking queue",
			slog.String("user_id", msg.UserID),
			slog.String("chat_id", msg.ChatID),
			slog.Int("queue_size", len(s.messageChan)))

		// Final attempt: blocking send with 30s max timeout (prevents goroutine leak)
		select {
		case s.messageChan <- msg:
			s.logger.Info("message queued after blocking",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID))
			return nil
		case <-ctx.Done():
			s.logger.Error("context cancelled during blocking queue attempt",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID))
			return ctx.Err()
		case <-time.After(30 * time.Second):
			s.logger.Error("message queue blocked for 35s total, giving up",
				slog.String("user_id", msg.UserID),
				slog.String("chat_id", msg.ChatID))
			return fmt.Errorf("message queue blocked for 35s total, giving up")
		}
	}
}

// Shutdown gracefully shuts down the service
func (s *Service) Shutdown() {
	s.logger.Info("shutting down message storage service")
	s.closed.Store(true)
	close(s.shutdown)
	s.workerPool.Wait()
	close(s.messageChan)
	s.logger.Info("message storage service shutdown complete")
}

// GetPublicKey exposes getPublicKey for title service
func (s *Service) GetPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	return s.getPublicKey(ctx, userID)
}

// EncryptContent exposes encryption function for title service
func (s *Service) EncryptContent(content string, publicKeyJWK string) (string, error) {
	return s.encryptionService.EncryptMessage(content, publicKeyJWK)
}

// SaveResponseID stores the latest OpenAI Responses API response_id for a chat.
// This is used for continuing conversations with GPT-5 Pro and other stateful models.
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: User ID who owns the chat
//   - chatID: Chat ID
//   - responseID: The response_id from OpenAI (e.g., "resp_abc123")
//
// Returns:
//   - error: If save failed
func (s *Service) SaveResponseID(ctx context.Context, userID, chatID, responseID string) error {
	if s.firestoreClient == nil {
		return fmt.Errorf("firestore client is nil")
	}
	return s.firestoreClient.SaveResponseID(ctx, userID, chatID, responseID)
}

// GetResponseID retrieves the latest OpenAI Responses API response_id for a chat.
// This is used for continuing conversations with GPT-5 Pro and other stateful models.
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: User ID who owns the chat
//   - chatID: Chat ID
//
// Returns:
//   - string: The response_id (e.g., "resp_abc123"), or empty string if not found
//   - error: If retrieval failed
func (s *Service) GetResponseID(ctx context.Context, userID, chatID string) (string, error) {
	if s.firestoreClient == nil {
		return "", fmt.Errorf("firestore client is nil")
	}
	return s.firestoreClient.GetResponseID(ctx, userID, chatID)
}

// SaveThinkingMessage saves a placeholder message for long-running generations (GPT-5 Pro).
// This allows clients to detect in-progress generation when reconnecting.
//
// The message is saved immediately when streaming starts with:
//   - generationState: "thinking"
//   - encryptedContent: empty (will be updated when complete)
//   - generationStartedAt: current timestamp
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: User ID who owns the message
//   - chatID: Chat ID
//   - messageID: AI message ID
//   - model: Model ID (e.g., "gpt-5-pro")
//   - encryptionEnabled: Whether to encrypt (can be nil for backward compat)
//
// Returns:
//   - error: If save failed
func (s *Service) SaveThinkingMessage(ctx context.Context, userID, chatID, messageID, model string, encryptionEnabled *bool) error {
	// Handle encryption for placeholder content
	var encryptedContent string
	var publicKeyUsed string

	if encryptionEnabled != nil && *encryptionEnabled {
		// Client wants encryption - encrypt empty placeholder
		publicKey, err := s.getPublicKey(ctx, userID)
		if err != nil {
			// Graceful degradation or strict mode based on config
			if config.AppConfig.MessageStorageRequireEncryption {
				return fmt.Errorf("encryption required but no public key found: %w", err)
			}
			encryptedContent = "" // Empty placeholder
			publicKeyUsed = "none"
		} else {
			// Encrypt empty placeholder content
			encrypted, err := s.encryptionService.EncryptMessage("", publicKey.Public)
			if err != nil {
				if config.AppConfig.MessageStorageRequireEncryption {
					return fmt.Errorf("encryption failed: %w", err)
				}
				encryptedContent = ""
				publicKeyUsed = "none"
			} else {
				encryptedContent = encrypted
				publicKeyUsed = publicKey.Public
			}
		}
	} else {
		// No encryption requested
		encryptedContent = ""
		publicKeyUsed = "none"
	}

	// Create placeholder message with "thinking" state
	now := time.Now()
	chatMsg := &ChatMessage{
		ID:                  messageID,
		EncryptedContent:    encryptedContent,
		IsFromUser:          false,
		ChatID:              chatID,
		IsError:             false,
		Timestamp:           now,
		PublicEncryptionKey: publicKeyUsed,
		Model:               model,
		GenerationState:     "thinking",
		GenerationStartedAt: now,
	}

	// Save to Firestore
	return s.firestoreClient.SaveMessage(ctx, userID, chatMsg)
}

// UpdateMessageGenerationState updates a message's generation state.
// Used to mark messages as "completed" or "failed" after generation finishes.
//
// This method updates an existing message in Firestore - it does NOT create a new message.
// The full message content should already be stored via the normal StoreMessageAsync flow.
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: User ID who owns the message
//   - chatID: Chat ID
//   - messageID: AI message ID
//   - state: New state ("completed" or "failed")
//   - errorMsg: Error message (only if state="failed")
//
// Returns:
//   - error: If update failed
func (s *Service) UpdateMessageGenerationState(ctx context.Context, userID, chatID, messageID, state, errorMsg string) error {
	now := time.Now()
	updates := map[string]interface{}{
		"generationState":       state,
		"generationCompletedAt": now,
	}

	if errorMsg != "" {
		updates["generationError"] = errorMsg
	}

	// Update in Firestore
	return s.firestoreClient.UpdateMessage(ctx, userID, chatID, messageID, updates)
}

// UpdateGenerationStateSync updates a message's generation state synchronously.
//
// This is used by the background polling worker to update Firestore state as
// OpenAI's response status changes.
//
// Unlike StoreMessageAsync, this method updates Firestore directly without
// going through the async worker queue. This ensures critical state transitions
// (thinking → completed/failed) are saved immediately.
//
// Parameters:
//   - ctx: Context for the operation
//   - userID: User ID who owns the message
//   - chatID: Chat ID
//   - messageID: AI message ID
//   - state: New state ("thinking", "completed", "failed")
//   - errorMsg: Error message (only if state="failed")
//
// Returns:
//   - error: If update failed
func (s *Service) UpdateGenerationStateSync(ctx context.Context, userID, chatID, messageID, state, errorMsg string) error {
	now := time.Now()
	updates := map[string]interface{}{
		"generationState": state,
		"updatedAt":       now,
	}

	// Add completion timestamp for terminal states
	if state == "completed" || state == "failed" {
		updates["generationCompletedAt"] = now
	}

	// Add error message if provided
	if errorMsg != "" {
		updates["generationError"] = errorMsg
	}

	s.logger.Debug("updating generation state synchronously",
		slog.String("user_id", userID),
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID),
		slog.String("state", state))

	// Update in Firestore synchronously (not through async queue)
	return s.firestoreClient.UpdateMessage(ctx, userID, chatID, messageID, updates)
}
