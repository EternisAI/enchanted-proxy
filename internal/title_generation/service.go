package title_generation

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
)

// Service handles async title generation with encryption
type Service struct {
	logger          *logger.Logger
	generator       *Generator
	messageService  *messaging.Service
	firestoreClient *messaging.FirestoreClient
	storageChan     chan StorageRequest
	workerPool      sync.WaitGroup
	shutdown        chan struct{}
	closed          atomic.Bool
}

// NewService creates a new title generation service
func NewService(
	logger *logger.Logger,
	generator *Generator,
	messageService *messaging.Service,
	firestoreClient *messaging.FirestoreClient,
) *Service {
	s := &Service{
		logger:          logger,
		generator:       generator,
		messageService:  messageService,
		firestoreClient: firestoreClient,
		storageChan:     make(chan StorageRequest, 100),
		shutdown:        make(chan struct{}),
	}

	// Start worker pool for storage operations
	const workerPoolSize = 2
	for i := 0; i < workerPoolSize; i++ {
		s.workerPool.Add(1)
		go s.storageWorker()
	}

	logger.Info("title generation service started", slog.Int("worker_pool_size", workerPoolSize))
	return s
}

// storageWorker processes title storage requests
func (s *Service) storageWorker() {
	defer s.workerPool.Done()

	for {
		select {
		case req := <-s.storageChan:
			s.storeTitle(req)
		case <-s.shutdown:
			// Drain remaining jobs
			for {
				select {
				case req := <-s.storageChan:
					s.storeTitle(req)
				default:
					return
				}
			}
		}
	}
}

// storeTitle encrypts and saves a title to Firestore
func (s *Service) storeTitle(req StorageRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log := s.logger.WithContext(ctx)

	log.Debug("storing title",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID))

	chatTitle := s.buildChatTitle(ctx, req, log)
	if chatTitle == nil {
		return
	}

	if err := s.firestoreClient.SaveChatTitle(ctx, req.UserID, req.ChatID, chatTitle); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "FailedPrecondition") {
			log.Warn("chat document not found - client hasn't created it yet",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))
			return
		}
		log.Error("failed to save title",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID),
			slog.String("error", err.Error()))
		return
	}

	log.Info("title saved",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID),
		slog.Bool("encrypted", chatTitle.EncryptedTitle != ""))
}

// buildChatTitle creates a ChatTitle with proper encryption based on settings
func (s *Service) buildChatTitle(ctx context.Context, req StorageRequest, log *logger.Logger) *messaging.ChatTitle {
	// Case 1: Client explicitly requests encryption
	if req.EncryptionEnabled != nil && *req.EncryptionEnabled {
		return s.buildEncryptedTitle(ctx, req, log, true)
	}

	// Case 2: Client explicitly disables encryption
	if req.EncryptionEnabled != nil && !*req.EncryptionEnabled {
		log.Info("storing plaintext title (encryption disabled)",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID))
		return &messaging.ChatTitle{
			Title:     req.Title,
			UpdatedAt: time.Now(),
		}
	}

	// Case 3: Backward compatibility - check if user has public key
	return s.buildEncryptedTitle(ctx, req, log, false)
}

// buildEncryptedTitle attempts to encrypt the title
func (s *Service) buildEncryptedTitle(ctx context.Context, req StorageRequest, log *logger.Logger, strict bool) *messaging.ChatTitle {
	publicKey, err := s.messageService.GetPublicKey(ctx, req.UserID)
	if err != nil {
		if strict {
			log.Error("encryption required but failed to get public key",
				slog.String("user_id", req.UserID),
				slog.String("error", err.Error()))
			return nil
		}
		log.Warn("failed to get public key, storing plaintext",
			slog.String("user_id", req.UserID),
			slog.String("error", err.Error()))
		return &messaging.ChatTitle{
			Title:     req.Title,
			UpdatedAt: time.Now(),
		}
	}

	if publicKey == nil || publicKey.Public == "" {
		if strict {
			log.Error("encryption required but public key is empty",
				slog.String("user_id", req.UserID))
			return nil
		}
		log.Info("no public key found, storing plaintext",
			slog.String("user_id", req.UserID))
		return &messaging.ChatTitle{
			Title:     req.Title,
			UpdatedAt: time.Now(),
		}
	}

	encrypted, err := s.messageService.EncryptContent(req.Title, publicKey.Public)
	if err != nil {
		if strict {
			log.Error("encryption required but failed",
				slog.String("user_id", req.UserID),
				slog.String("error", err.Error()))
			return nil
		}
		log.Error("encryption failed, refusing to save plaintext when user has key",
			slog.String("user_id", req.UserID),
			slog.String("error", err.Error()))
		return nil
	}

	log.Info("title encrypted",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID))

	return &messaging.ChatTitle{
		EncryptedTitle:           encrypted,
		TitlePublicEncryptionKey: publicKey.Public,
		UpdatedAt:                time.Now(),
	}
}

// queueStorage queues a title for encryption and storage
func (s *Service) queueStorage(ctx context.Context, req StorageRequest) {
	if s.closed.Load() {
		s.logger.Warn("service shutting down, cannot queue storage")
		return
	}

	log := s.logger.WithContext(ctx)

	select {
	case s.storageChan <- req:
		log.Debug("title queued for storage", slog.String("chat_id", req.ChatID))
	case <-ctx.Done():
		log.Warn("context cancelled, cannot queue storage")
	case <-time.After(5 * time.Second):
		if s.closed.Load() {
			return
		}
		log.Error("storage queue blocked for 5s",
			slog.String("chat_id", req.ChatID),
			slog.Int("queue_size", len(s.storageChan)))
		// Final blocking attempt with timeout
		select {
		case s.storageChan <- req:
			log.Info("title queued after blocking", slog.String("chat_id", req.ChatID))
		case <-ctx.Done():
			log.Error("context cancelled during blocking queue")
		case <-time.After(30 * time.Second):
			log.Error("storage queue blocked for 35s total, giving up")
		}
	}
}

// GenerateAndStore generates a title from first message and queues it for storage
func (s *Service) GenerateAndStore(ctx context.Context, genReq GenerateRequest, storeReq StorageRequest) {
	if s.closed.Load() {
		s.logger.Warn("service shutting down, cannot generate title")
		return
	}

	log := s.logger.WithContext(ctx)

	log.Info("generating initial title",
		slog.String("chat_id", storeReq.ChatID),
		slog.String("model", genReq.Model),
		slog.Int("content_length", len(genReq.UserContent)))

	title, err := s.generator.GenerateInitial(ctx, genReq)
	if err != nil {
		log.Error("failed to generate title",
			slog.String("error", err.Error()),
			slog.String("chat_id", storeReq.ChatID))
		return
	}

	log.Info("title generated",
		slog.String("title", title),
		slog.String("chat_id", storeReq.ChatID))

	storeReq.Title = title
	s.queueStorage(ctx, storeReq)
}

// RegenerateAndStore regenerates a title with context and queues it for storage
func (s *Service) RegenerateAndStore(ctx context.Context, genReq GenerateRequest, regenCtx RegenerationContext, storeReq StorageRequest) {
	if s.closed.Load() {
		s.logger.Warn("service shutting down, cannot regenerate title")
		return
	}

	log := s.logger.WithContext(ctx)

	log.Info("regenerating title with context",
		slog.String("chat_id", storeReq.ChatID),
		slog.String("model", genReq.Model),
		slog.Int("first_msg_len", len(regenCtx.FirstUserMessage)),
		slog.Int("ai_response_len", len(regenCtx.FirstAIResponse)),
		slog.Int("second_msg_len", len(regenCtx.SecondUserMessage)))

	title, err := s.generator.GenerateFromContext(ctx, genReq, regenCtx)
	if err != nil {
		log.Error("failed to regenerate title",
			slog.String("error", err.Error()),
			slog.String("chat_id", storeReq.ChatID))
		return
	}

	log.Info("title regenerated",
		slog.String("title", title),
		slog.String("chat_id", storeReq.ChatID))

	storeReq.Title = title
	s.queueStorage(ctx, storeReq)
}

// Shutdown gracefully shuts down the service
func (s *Service) Shutdown() {
	s.logger.Info("shutting down title generation service")
	s.closed.Store(true)
	close(s.shutdown)
	s.workerPool.Wait()
	close(s.storageChan)
	s.logger.Info("title generation service shutdown complete")
}
