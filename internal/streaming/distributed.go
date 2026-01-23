package streaming

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/nats-io/nats.go"
)

const (
	// NATS subject for stream cancellation requests
	streamCancelSubject = "stream.cancel"

	// Timeout for distributed cancel requests
	distributedCancelTimeout = 5 * time.Second
)

// CancelRequest represents a distributed stream cancellation request.
type CancelRequest struct {
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
	UserID    string `json:"user_id"`
	Reason    string `json:"reason"`
}

// CancelResponse represents the result of a distributed cancel operation.
type CancelResponse struct {
	Success         bool   `json:"success"`
	Found           bool   `json:"found"`
	AlreadyStopped  bool   `json:"already_stopped,omitempty"`
	AlreadyComplete bool   `json:"already_complete,omitempty"`
	ChunksGenerated int    `json:"chunks_generated,omitempty"`
	Error           string `json:"error,omitempty"`
	InstanceID      string `json:"instance_id"`
}

// DistributedCancelService handles cross-instance stream cancellation via NATS.
//
// In a multi-instance deployment, streaming sessions are stored in-memory on the
// instance that initiated the upstream request. When a stop request arrives at a
// different instance, this service broadcasts the cancel signal via NATS pub/sub,
// allowing the owning instance to stop the stream.
//
// Architecture:
//
//	Instance A (owns session)              Instance B (receives /stop)
//	─────────────────────────              ───────────────────────────
//	                                       POST /stop arrives
//	                                         └─► Local session not found
//	                                         └─► Publish cancel request to NATS
//	◄─── NATS delivers request ────
//	  └─► Find local session
//	  └─► Stop stream
//	  └─► Reply with result ────────────►
//	                                         └─► Return response to client
type DistributedCancelService struct {
	nc           *nats.Conn
	manager      *StreamManager
	logger       *logger.Logger
	instanceID   string
	subscription *nats.Subscription
}

// NewDistributedCancelService creates a new distributed cancel service.
// Returns nil if NATS connection is not available.
func NewDistributedCancelService(nc *nats.Conn, manager *StreamManager, logger *logger.Logger, instanceID string) *DistributedCancelService {
	if nc == nil {
		return nil
	}

	return &DistributedCancelService{
		nc:         nc,
		manager:    manager,
		logger:     logger.WithComponent("distributed-cancel"),
		instanceID: instanceID,
	}
}

// Start begins listening for distributed cancel requests.
// This should be called once during server startup.
func (s *DistributedCancelService) Start() error {
	sub, err := s.nc.Subscribe(streamCancelSubject, s.handleCancelRequest)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", streamCancelSubject, err)
	}

	s.subscription = sub
	s.logger.Info("distributed cancel service started",
		slog.String("subject", streamCancelSubject),
		slog.String("instance_id", s.instanceID))

	return nil
}

// Stop gracefully shuts down the service.
func (s *DistributedCancelService) Stop() error {
	if s.subscription != nil {
		if err := s.subscription.Drain(); err != nil {
			return fmt.Errorf("failed to drain subscription: %w", err)
		}
	}
	s.logger.Info("distributed cancel service stopped")
	return nil
}

// RequestCancel sends a cancel request to all instances and waits for a response.
// Returns the response from the instance that owns the session, or an error if
// no instance responds within the timeout.
func (s *DistributedCancelService) RequestCancel(ctx context.Context, chatID, messageID, userID string) (*CancelResponse, error) {
	req := CancelRequest{
		ChatID:    chatID,
		MessageID: messageID,
		UserID:    userID,
		Reason:    string(StopReasonUserCancelled),
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create context with timeout for NATS request
	reqCtx, cancel := context.WithTimeout(ctx, distributedCancelTimeout)
	defer cancel()

	// Use NATS request-reply pattern with context
	msg, err := s.nc.RequestWithContext(reqCtx, streamCancelSubject, data)
	if err != nil {
		// No subscribers on the subject
		if errors.Is(err, nats.ErrNoResponders) {
			return &CancelResponse{
				Success: false,
				Found:   false,
			}, nil
		}
		// Timeout - no instance owns this session
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, nats.ErrTimeout) {
			return &CancelResponse{
				Success: false,
				Found:   false,
			}, nil
		}
		// Context cancelled by caller
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("cancel request failed: %w", err)
	}

	var resp CancelResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &resp, nil
}

// handleCancelRequest processes incoming cancel requests from other instances.
// Only responds if this instance owns the session - otherwise stays silent to allow
// the owning instance to respond.
func (s *DistributedCancelService) handleCancelRequest(msg *nats.Msg) {
	var req CancelRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		// Invalid request - log but don't reply (we might not be the intended recipient)
		s.logger.Warn("received invalid cancel request", slog.String("error", err.Error()))
		return
	}

	s.logger.Debug("received cancel request",
		slog.String("chat_id", req.ChatID),
		slog.String("message_id", req.MessageID))

	// Check if we own this session
	session := s.manager.GetSession(req.ChatID, req.MessageID)
	if session == nil {
		// We don't have this session - don't respond (let the owning instance handle it)
		s.logger.Debug("session not owned by this instance, ignoring",
			slog.String("chat_id", req.ChatID),
			slog.String("message_id", req.MessageID))
		return
	}

	// We own the session - process the cancel and reply
	resp := s.processLocalCancel(session, req)
	resp.InstanceID = s.instanceID

	s.reply(msg, resp)

	s.logger.Info("processed distributed cancel request",
		slog.String("chat_id", req.ChatID),
		slog.String("message_id", req.MessageID),
		slog.Bool("success", resp.Success))
}

// processLocalCancel stops a local session and returns the result.
func (s *DistributedCancelService) processLocalCancel(session *StreamSession, req CancelRequest) CancelResponse {
	// Check if already completed
	if session.IsCompleted() {
		return CancelResponse{
			Success:         false,
			Found:           true,
			AlreadyComplete: true,
		}
	}

	// Attempt to stop
	err := session.Stop(req.UserID, StopReason(req.Reason))
	if err != nil {
		if err.Error() == "stream already stopped" {
			return CancelResponse{
				Success:        false,
				Found:          true,
				AlreadyStopped: true,
			}
		}
		return CancelResponse{
			Success: false,
			Found:   true,
			Error:   err.Error(),
		}
	}

	chunks := session.GetStoredChunks()
	return CancelResponse{
		Success:         true,
		Found:           true,
		ChunksGenerated: len(chunks),
	}
}

// reply sends a response back to the requester.
func (s *DistributedCancelService) reply(msg *nats.Msg, resp CancelResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		s.logger.Error("failed to marshal response", slog.String("error", err.Error()))
		return
	}

	if err := msg.Respond(data); err != nil {
		s.logger.Error("failed to send response", slog.String("error", err.Error()))
	}
}
