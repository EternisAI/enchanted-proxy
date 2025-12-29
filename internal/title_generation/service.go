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
	messageService  *messaging.Service // For encryption
	firestoreClient *messaging.FirestoreClient
	titleChan       chan TitleGenerationRequest
	workerPool      sync.WaitGroup
	shutdown        chan struct{}
	closed          atomic.Bool
}

// NewService creates a new title generation service
func NewService(logger *logger.Logger, messageService *messaging.Service, firestoreClient *messaging.FirestoreClient) *Service {
	s := &Service{
		logger:          logger,
		messageService:  messageService,
		firestoreClient: firestoreClient,
		titleChan:       make(chan TitleGenerationRequest, 100), // Buffer for title gen jobs
		shutdown:        make(chan struct{}),
	}

	// Start worker pool (fewer workers than message storage)
	workerPoolSize := 2 // Title generation is less frequent
	for i := 0; i < workerPoolSize; i++ {
		s.workerPool.Add(1)
		go s.worker()
	}

	logger.Info("title generation service started", slog.Int("worker_pool_size", workerPoolSize))

	return s
}

// worker processes title generation requests
func (s *Service) worker() {
	defer s.workerPool.Done()

	for {
		select {
		case req := <-s.titleChan:
			s.handleTitleGeneration(req)
		case <-s.shutdown:
			// Drain remaining jobs
			for {
				select {
				case req := <-s.titleChan:
					s.handleTitleGeneration(req)
				default:
					return
				}
			}
		}
	}
}

// handleTitleGeneration processes a single title generation request
func (s *Service) handleTitleGeneration(req TitleGenerationRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	log := s.logger.WithContext(ctx)

	log.Debug("generating title",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID),
		slog.String("model", req.Model))

	// Handle encryption based on client's explicit signal (same logic as message storage)
	var chatTitle *messaging.ChatTitle

	// Case 1: Client explicitly requests encryption (encryptionEnabled = true)
	if req.EncryptionEnabled != nil && *req.EncryptionEnabled {
		log.Debug("encryption explicitly enabled by client for title",
			slog.String("user_id", req.UserID))

		publicKey, err := s.messageService.GetPublicKey(ctx, req.UserID)
		if err != nil {
			// STRICT MODE: If client expects encryption, we MUST encrypt
			log.Error("encryption enabled but failed to get public key for title",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			return // Fail: don't store if client expects encryption
		}
		if publicKey == nil || publicKey.Public == "" {
			log.Error("encryption enabled but public key is empty for title",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))
			return // Fail: don't store if client expects encryption
		}

		encrypted, err := s.messageService.EncryptContent(req.FirstMessage, publicKey.Public)
		if err != nil {
			log.Error("title encryption failed (client expects encryption)",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			return // Fail: don't store if encryption fails
		}

		// ENCRYPTED: Use encryptedTitle field only
		chatTitle = &messaging.ChatTitle{
			EncryptedTitle:           encrypted,
			TitlePublicEncryptionKey: publicKey.Public,
			UpdatedAt:                time.Now(),
		}
		log.Info("title encrypted per client request",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID))
	} else if req.EncryptionEnabled != nil && !*req.EncryptionEnabled {
		// Case 2: Client explicitly disables encryption (encryptionEnabled = false)
		log.Info("encryption explicitly disabled by client for title, storing plaintext",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID))

		// PLAINTEXT: Use title field only
		chatTitle = &messaging.ChatTitle{
			Title:     req.FirstMessage,
			UpdatedAt: time.Now(),
		}
	} else {
		// Case 3: Backward compatibility - header not provided (encryptionEnabled = nil)
		// IMPORTANT: If user has public key, we MUST encrypt (no fallback to plaintext)
		// Only save plaintext if user has NOT set up encryption
		log.Debug("encryption header not provided for title, using backward compatible logic",
			slog.String("user_id", req.UserID))

		publicKey, err := s.messageService.GetPublicKey(ctx, req.UserID)
		if err != nil {
			// Failed to fetch public key - check if it's "not found" vs other error
			log.Warn("failed to fetch public key for title (backward compat mode)",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID),
				slog.String("error", err.Error()))
			// Assume no public key set up - save plaintext
			chatTitle = &messaging.ChatTitle{
				Title:     req.FirstMessage,
				UpdatedAt: time.Now(),
			}
		} else if publicKey == nil || publicKey.Public == "" {
			// User has NOT set up encryption - save plaintext
			log.Info("no public key found for user, saving title as plaintext",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))
			chatTitle = &messaging.ChatTitle{
				Title:     req.FirstMessage,
				UpdatedAt: time.Now(),
			}
		} else {
			// User HAS public key - MUST encrypt (no fallback)
			log.Info("public key found for user, encrypting title (backward compat mode)",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))

			encrypted, err := s.messageService.EncryptContent(req.FirstMessage, publicKey.Public)
			if err != nil {
				log.Error("title encryption failed and user HAS public key - refusing to save plaintext",
					slog.String("user_id", req.UserID),
					slog.String("chat_id", req.ChatID),
					slog.String("error", err.Error()))
				return // FAIL: Don't save plaintext when encryption is expected
			}

			// ENCRYPTED: Use encryptedTitle field only
			chatTitle = &messaging.ChatTitle{
				EncryptedTitle:           encrypted,
				TitlePublicEncryptionKey: publicKey.Public,
				UpdatedAt:                time.Now(),
			}
			log.Info("title encrypted successfully (backward compat mode)",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))
		}
	}

	if err := s.firestoreClient.SaveChatTitle(ctx, req.UserID, req.ChatID, chatTitle); err != nil {
		// Check if error is due to chat document not existing yet
		// This is expected if title generation completes before client creates the chat doc
		errMsg := err.Error()
		if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "FailedPrecondition") {
			log.Warn("chat document not found - client hasn't created it yet, title will be set by client",
				slog.String("user_id", req.UserID),
				slog.String("chat_id", req.ChatID))
			return
		}

		// Unexpected error
		log.Error("failed to save title to firestore",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID),
			slog.String("error", err.Error()))
		return
	}

	log.Info("title saved successfully",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID),
		slog.Bool("encrypted", chatTitle.EncryptedTitle != ""))
}

// QueueTitleGeneration queues a title generation request
func (s *Service) QueueTitleGeneration(ctx context.Context, req TitleGenerationRequest, apiKey string) {
	if s.closed.Load() {
		s.logger.Warn("service is shutting down, cannot queue title generation")
		return
	}

	log := s.logger.WithContext(ctx)

	// Generate title via AI first (blocking, but fast)
	log.Info("starting title generation",
		slog.String("chat_id", req.ChatID),
		slog.String("model", req.Model),
		slog.String("base_url", req.BaseURL),
		slog.Int("message_length", len(req.FirstMessage)))

	title, err := GenerateTitle(ctx, req, apiKey)
	if err != nil {
		log.Error("failed to generate title",
			slog.String("error", err.Error()),
			slog.String("chat_id", req.ChatID),
			slog.String("model", req.Model),
			slog.String("base_url", req.BaseURL))
		return
	}

	log.Info("title generated successfully",
		slog.String("title", title),
		slog.String("chat_id", req.ChatID),
		slog.String("model", req.Model))

	// Update request with generated title
	req.FirstMessage = title

	// Check again if service is shutting down (after AI call which could take time)
	// Prevents panic from sending to closed channel
	if s.closed.Load() {
		log.Warn("service is shutting down after title generation, cannot queue",
			slog.String("chat_id", req.ChatID))
		return
	}

	// Queue for encryption and storage
	// Wait up to 5 seconds for queue space (no silent drops)
	select {
	case s.titleChan <- req:
		log.Debug("title generation queued", slog.String("chat_id", req.ChatID))
	case <-ctx.Done():
		log.Warn("context cancelled, cannot queue title generation")
	case <-time.After(5 * time.Second):
		// Queue blocked for 5 seconds - this indicates a serious problem
		// Check once more if shutting down before blocking
		if s.closed.Load() {
			log.Warn("service is shutting down, cannot queue after timeout",
				slog.String("chat_id", req.ChatID))
			return
		}

		// Log as error and try one more time with limited timeout
		log.Error("title generation queue blocked for 5s, attempting blocking queue",
			slog.String("chat_id", req.ChatID),
			slog.Int("queue_size", len(s.titleChan)))

		// Final attempt: blocking send with 30s max timeout (prevents goroutine leak)
		select {
		case s.titleChan <- req:
			log.Info("title generation queued after blocking", slog.String("chat_id", req.ChatID))
		case <-ctx.Done():
			log.Error("context cancelled during blocking queue attempt", slog.String("chat_id", req.ChatID))
		case <-time.After(30 * time.Second):
			log.Error("title generation queue blocked for 35s total, giving up",
				slog.String("chat_id", req.ChatID))
		}
	}
}

// Shutdown gracefully shuts down the service
func (s *Service) Shutdown() {
	s.logger.Info("shutting down title generation service")
	s.closed.Store(true)
	close(s.shutdown)
	s.workerPool.Wait()
	close(s.titleChan)
	s.logger.Info("title generation service shutdown complete")
}
