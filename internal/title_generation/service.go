package title_generation

import (
	"context"
	"fmt"
	"log/slog"
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

	// Get API key for this base URL - will be provided by caller
	// The GetAPIKey function will be exposed from proxy package

	// Encrypt title (same as messages)
	encryptedTitle, publicKeyUsed, err := s.encryptTitle(ctx, req.UserID, req.FirstMessage)
	if err != nil {
		log.Error("failed to encrypt title", slog.String("error", err.Error()))
		// Continue with plaintext if encryption fails (graceful degradation)
		encryptedTitle = req.FirstMessage
		publicKeyUsed = "none"
	}

	// Save to Firestore
	chatTitle := &messaging.ChatTitle{
		EncryptedTitle:           encryptedTitle,
		TitlePublicEncryptionKey: publicKeyUsed,
		UpdatedAt:                time.Now(),
	}

	if err := s.firestoreClient.SaveChatTitle(ctx, req.UserID, req.ChatID, chatTitle); err != nil {
		log.Error("failed to save title to firestore",
			slog.String("user_id", req.UserID),
			slog.String("chat_id", req.ChatID),
			slog.String("error", err.Error()))
		return
	}

	log.Info("title saved successfully",
		slog.String("user_id", req.UserID),
		slog.String("chat_id", req.ChatID),
		slog.Bool("encrypted", publicKeyUsed != "none"))
}

// encryptTitle encrypts a title using user's public key
func (s *Service) encryptTitle(ctx context.Context, userID, title string) (string, string, error) {
	// Reuse message encryption infrastructure
	publicKey, err := s.messageService.GetPublicKey(ctx, userID)
	if err != nil {
		return "", "", err
	}

	if publicKey == nil || publicKey.Public == "" {
		return "", "", fmt.Errorf("no public key available")
	}

	// Use message encryption service
	encrypted, err := s.messageService.EncryptContent(title, publicKey.Public)
	if err != nil {
		return "", "", err
	}

	return encrypted, publicKey.Public, nil
}

// QueueTitleGeneration queues a title generation request
func (s *Service) QueueTitleGeneration(ctx context.Context, req TitleGenerationRequest, apiKey string) {
	if s.closed.Load() {
		s.logger.Warn("service is shutting down, cannot queue title generation")
		return
	}

	log := s.logger.WithContext(ctx)

	// Generate title via AI first (blocking, but fast)
	title, err := GenerateTitle(ctx, req, apiKey)
	if err != nil {
		log.Error("failed to generate title", slog.String("error", err.Error()))
		return
	}

	log.Debug("title generated", slog.String("title", title))

	// Update request with generated title
	req.FirstMessage = title

	// Queue for encryption and storage
	select {
	case s.titleChan <- req:
		log.Debug("title generation queued", slog.String("chat_id", req.ChatID))
	case <-ctx.Done():
		log.Warn("context cancelled, cannot queue title generation")
	default:
		log.Warn("title generation queue full, dropping request", slog.String("chat_id", req.ChatID))
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
