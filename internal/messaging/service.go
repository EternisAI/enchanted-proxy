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
	publicKeyCache    *PublicKeyCache
}

// NewService creates a new message storage service
func NewService(firestoreClient *firestore.Client, logger *logger.Logger) *Service {
	s := &Service{
		firestoreClient:   NewFirestoreClient(firestoreClient),
		encryptionService: NewEncryptionService(),
		logger:            logger,
		messageChan:       make(chan MessageToStore, config.AppConfig.MessageStorageBufferSize), // Buffered channel to queue messages waiting for workers
		shutdown:          make(chan struct{}),
		publicKeyCache:    NewPublicKeyCache(config.AppConfig.MessageStorageCacheSize, time.Duration(config.AppConfig.MessageStorageCacheTTLMinutes)*time.Minute), // Cache user public keys to reduce Firestore reads
	}

	// Start worker pool - each worker processes messages concurrently from the queue
	for i := 0; i < config.AppConfig.MessageStorageWorkerPoolSize; i++ {
		s.workerPool.Add(1)
		go s.worker()
	}

	logger.Info("message storage service started",
		slog.Int("worker_pool_size", config.AppConfig.MessageStorageWorkerPoolSize),
		slog.Int("buffer_size", config.AppConfig.MessageStorageBufferSize),
		slog.Int("cache_size", config.AppConfig.MessageStorageCacheSize),
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

	// Encrypt message content
	var encryptedContent string
	var publicKeyUsed string

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

	// Create Firestore message
	chatMsg := &ChatMessage{
		ID:                  msg.MessageID,
		EncryptedContent:    encryptedContent,
		IsFromUser:          msg.IsFromUser,
		ChatID:              msg.ChatID,
		IsError:             msg.IsError,
		Timestamp:           time.Now(),
		PublicEncryptionKey: publicKeyUsed,
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

// getPublicKey retrieves public key with caching
func (s *Service) getPublicKey(ctx context.Context, userID string) (*UserPublicKey, error) {
	// Check cache
	if key := s.publicKeyCache.Get(userID); key != nil {
		return key, nil
	}

	// Fetch from Firestore
	key, err := s.firestoreClient.GetUserPublicKey(ctx, userID)
	if err != nil {
		return nil, err
	}

	// Check if key is valid
	if key == nil || key.Public == "" {
		return nil, fmt.Errorf("no public key found for user %s", userID)
	}

	// Validate key
	if err := s.encryptionService.ValidatePublicKey(key.Public); err != nil {
		return nil, fmt.Errorf("invalid public key: %w", err)
	}

	// Cache it
	s.publicKeyCache.Set(userID, key)

	return key, nil
}

// StoreMessageAsync queues a message for async storage
func (s *Service) StoreMessageAsync(ctx context.Context, msg MessageToStore) error {
	if s.closed.Load() {
		return fmt.Errorf("service is shutting down")
	}

	select {
	case s.messageChan <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		s.logger.Warn("message queue is full, dropping message",
			slog.String("user_id", msg.UserID),
			slog.String("chat_id", msg.ChatID))
		return fmt.Errorf("message queue is full")
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
