package keyshare

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// RateLimitSessionsPerHour is the maximum number of sessions a user can create per hour
	RateLimitSessionsPerHour = 10

	// SessionExpirationMinutes is the number of minutes until a session expires
	SessionExpirationMinutes = 5

	// MaxConcurrentWebSocketsPerUser is the maximum number of concurrent WebSocket connections per user
	MaxConcurrentWebSocketsPerUser = 3
)

// Service handles business logic for key sharing
type Service struct {
	firestoreClient  *FirestoreClient
	websocketManager *WebSocketManager
	logger           *logger.Logger
}

// NewService creates a new key sharing service
func NewService(firestoreClient *FirestoreClient, websocketManager *WebSocketManager, logger *logger.Logger) *Service {
	return &Service{
		firestoreClient:  firestoreClient,
		websocketManager: websocketManager,
		logger:           logger,
	}
}

// CreateSession creates a new key sharing session
func (s *Service) CreateSession(ctx context.Context, userID string, req CreateSessionRequest) (*CreateSessionResponse, error) {
	log := s.logger.WithContext(ctx).WithComponent("keyshare_service")

	// Validate ephemeral public key
	if err := s.validateEphemeralPublicKey(req.EphemeralPublicKey); err != nil {
		log.Error("invalid ephemeral public key",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// Check rate limit
	if err := s.checkRateLimit(ctx, userID); err != nil {
		log.Warn("rate limit exceeded",
			slog.String("user_id", userID))
		return nil, err
	}

	// Create session
	sessionID := uuid.New().String()
	now := time.Now()
	expiresAt := now.Add(SessionExpirationMinutes * time.Minute)

	session := &KeyShareSession{
		SessionID:          sessionID,
		UserID:             userID,
		EphemeralPublicKey: req.EphemeralPublicKey,
		Status:             SessionStatusPending,
		CreatedAt:          now,
		ExpiresAt:          expiresAt,
	}

	if err := s.firestoreClient.CreateSession(ctx, session); err != nil {
		log.Error("failed to create session",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return nil, status.Error(codes.Internal, "failed to create session")
	}

	log.Info("session created successfully",
		slog.String("user_id", userID),
		slog.String("session_id", sessionID),
		slog.Time("expires_at", expiresAt))

	return &CreateSessionResponse{
		SessionID: sessionID,
		ExpiresAt: expiresAt.Format(time.RFC3339),
	}, nil
}

// SubmitEncryptedKey submits an encrypted private key to a session
func (s *Service) SubmitEncryptedKey(ctx context.Context, userID, sessionID string, req SubmitKeyRequest) error {
	log := s.logger.WithContext(ctx).WithComponent("keyshare_service")

	// Get session
	session, err := s.firestoreClient.GetSession(ctx, sessionID)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.Warn("session not found",
				slog.String("user_id", userID),
				slog.String("session_id", sessionID))
			return status.Error(codes.NotFound, "session not found or expired")
		}
		log.Error("failed to get session",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return status.Error(codes.Internal, "failed to get session")
	}

	// Validate session ownership
	if session.UserID != userID {
		log.Warn("session ownership validation failed",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("session_owner", session.UserID))
		return status.Error(codes.PermissionDenied, "you don't own this session")
	}

	// Validate session status
	if err := s.validateSessionStatus(session); err != nil {
		log.Warn("session status validation failed",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("status", string(session.Status)),
			slog.String("error", err.Error()))
		return err
	}

	// Update session with encrypted key
	if err := s.firestoreClient.UpdateSessionWithKey(ctx, sessionID, req.EncryptedPrivateKey); err != nil {
		log.Error("failed to update session",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return status.Error(codes.Internal, "failed to update session")
	}

	// Broadcast to WebSocket listeners
	message := WebSocketMessage{
		Type:                WSMessageTypeKeyReceived,
		EncryptedPrivateKey: req.EncryptedPrivateKey,
	}
	if err := s.websocketManager.SendToSession(sessionID, message); err != nil {
		log.Error("failed to broadcast to websocket",
			slog.String("user_id", userID),
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		// Don't return error - session is already updated in Firestore
	}

	log.Info("encrypted key submitted successfully",
		slog.String("user_id", userID),
		slog.String("session_id", sessionID))

	return nil
}

// GetSession retrieves a session (for WebSocket validation)
func (s *Service) GetSession(ctx context.Context, sessionID string) (*KeyShareSession, error) {
	return s.firestoreClient.GetSession(ctx, sessionID)
}

// validateEphemeralPublicKey validates the ephemeral public key format
func (s *Service) validateEphemeralPublicKey(key EphemeralPublicKey) error {
	if key.Kty != "EC" {
		return fmt.Errorf("ephemeral public key must be EC (got %s)", key.Kty)
	}
	if key.Crv != "P-256" {
		return fmt.Errorf("ephemeral public key must use P-256 curve (got %s)", key.Crv)
	}
	if key.X == "" || key.Y == "" {
		return fmt.Errorf("ephemeral public key missing x or y coordinates")
	}
	// TODO: Could add more validation for base64url encoding format
	return nil
}

// checkRateLimit checks if the user has exceeded the rate limit
func (s *Service) checkRateLimit(ctx context.Context, userID string) error {
	count, err := s.firestoreClient.CountRecentSessions(ctx, userID)
	if err != nil {
		s.logger.WithContext(ctx).WithComponent("keyshare_service").Error("failed to count sessions",
			slog.String("user_id", userID),
			slog.String("error", err.Error()))
		// Don't fail request on rate limit check errors
		return nil
	}

	if count >= RateLimitSessionsPerHour {
		return status.Errorf(codes.ResourceExhausted, "maximum %d sessions per hour exceeded", RateLimitSessionsPerHour)
	}

	return nil
}

// validateSessionStatus validates that a session can receive a key
func (s *Service) validateSessionStatus(session *KeyShareSession) error {
	if session.Status == SessionStatusCompleted {
		return status.Error(codes.FailedPrecondition, "session already completed")
	}
	if session.Status == SessionStatusExpired {
		return status.Error(codes.FailedPrecondition, "session expired")
	}
	if time.Now().After(session.ExpiresAt) {
		return status.Error(codes.DeadlineExceeded, "session expired")
	}
	if session.Status != SessionStatusPending {
		return status.Errorf(codes.FailedPrecondition, "invalid session status: %s", session.Status)
	}
	return nil
}

// CleanupExpiredSessions deletes expired sessions (called by background job)
func (s *Service) CleanupExpiredSessions(ctx context.Context) (int, error) {
	log := s.logger.WithContext(ctx).WithComponent("keyshare_cleanup")

	const batchSize = 100
	deleted, err := s.firestoreClient.DeleteExpiredSessions(ctx, batchSize)
	if err != nil {
		log.Error("failed to delete expired sessions",
			slog.String("error", err.Error()))
		return 0, err
	}

	if deleted > 0 {
		log.Info("deleted expired sessions",
			slog.Int("count", deleted))
	}

	return deleted, nil
}
