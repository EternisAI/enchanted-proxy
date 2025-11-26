package streaming

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/messaging"
)

const (
	// sessionTTL is how long to keep completed sessions in memory
	// Allows late-joiners to access completed streams
	sessionTTL = 30 * time.Minute

	// cleanupInterval is how often to run cleanup of expired sessions
	cleanupInterval = 5 * time.Minute

	// maxMemoryBytes is the approximate memory limit for all buffered chunks
	// When exceeded, cleanup becomes more aggressive
	maxMemoryBytes = 500 * 1024 * 1024 // 500MB
)

// StreamManager manages the lifecycle of all active stream sessions.
//
// Responsibilities:
//   - Create new sessions when clients request AI responses
//   - Lookup existing sessions for multi-client broadcast
//   - Cleanup expired sessions to prevent memory leaks
//   - Provide observability (metrics, active streams list)
//   - Handle graceful shutdown
//
// Thread-safety:
//   - All public methods are thread-safe
//   - Uses RWMutex for read-heavy workload (many lookups, few creates)
//   - Safe for concurrent access from multiple HTTP handlers
type StreamManager struct {
	// sessions maps sessionKey ("chatID:messageID") to StreamSession
	sessions map[string]*StreamSession
	mu       sync.RWMutex

	// messageService handles storing completed messages to Firestore
	messageService *messaging.Service

	// toolExecutor handles tool call execution (optional)
	toolExecutor *ToolExecutor

	// logger for this manager
	logger *logger.Logger

	// cleanup goroutine management
	shutdownCleanup chan struct{}
	cleanupWg       sync.WaitGroup

	// metrics tracking
	metricsLock            sync.RWMutex
	totalSessionsCreated   int64
	totalSessionsCompleted int64
	totalSubscriptions     int64
}

// NewStreamManager creates a new stream manager.
//
// Parameters:
//   - messageService: Service for storing completed messages to Firestore (can be nil for testing)
//   - logger: Logger for this manager
//
// Returns:
//   - *StreamManager: The new manager (cleanup goroutine started automatically)
//
// The manager starts a background goroutine for cleanup.
// Call Shutdown() when done to stop the cleanup goroutine.
func NewStreamManager(messageService *messaging.Service, logger *logger.Logger) *StreamManager {
	sm := &StreamManager{
		sessions:        make(map[string]*StreamSession),
		messageService:  messageService,
		logger:          logger,
		shutdownCleanup: make(chan struct{}),
	}

	// Start cleanup goroutine
	sm.cleanupWg.Add(1)
	go sm.cleanupLoop()

	logger.Info("stream manager initialized")

	return sm
}

// GetOrCreateSession finds an existing session or creates a new one.
//
// Parameters:
//   - chatID: Chat session identifier
//   - messageID: AI response message identifier
//   - upstreamBody: Response body from AI provider (only used if creating new session)
//
// Returns:
//   - *StreamSession: Existing or new session
//   - bool: true if session was created (false if existing)
//
// Behavior:
//   - If session exists: Returns it immediately (for multi-client broadcast)
//   - If session doesn't exist: Creates new session, starts upstream read, returns it
//
// Thread-safe: Uses double-check locking to prevent duplicate session creation.
//
// Example usage:
//   session, isNew := manager.GetOrCreateSession(chatID, messageID, upstreamBody)
//   if isNew {
//       // First client for this response
//   } else {
//       // Additional client joining existing stream
//   }
func (sm *StreamManager) GetOrCreateSession(chatID, messageID string, upstreamBody io.ReadCloser) (*StreamSession, bool) {
	sessionKey := sm.makeSessionKey(chatID, messageID)

	// Fast path: Check if session already exists (read lock)
	sm.mu.RLock()
	if session, exists := sm.sessions[sessionKey]; exists {
		sm.mu.RUnlock()
		sm.logger.Debug("reusing existing stream session",
			slog.String("session_key", sessionKey),
			slog.Int("subscriber_count", session.GetSubscriberCount()))
		return session, false
	}
	sm.mu.RUnlock()

	// Slow path: Create new session (write lock with double-check)
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Double-check: Another goroutine might have created it while we waited for write lock
	if session, exists := sm.sessions[sessionKey]; exists {
		sm.logger.Debug("session created by another goroutine",
			slog.String("session_key", sessionKey))
		// Close the upstreamBody we won't use
		if upstreamBody != nil {
			upstreamBody.Close()
		}
		return session, false
	}

	// Create new session
	session := NewStreamSession(chatID, messageID, upstreamBody, sm.logger)
	sm.sessions[sessionKey] = session

	// Set tool executor if available
	if sm.toolExecutor != nil {
		session.SetToolExecutor(sm.toolExecutor)
	}

	// Start reading upstream in background
	session.Start()

	// Update metrics
	sm.metricsLock.Lock()
	sm.totalSessionsCreated++
	sm.metricsLock.Unlock()

	sm.logger.Info("created new stream session",
		slog.String("session_key", sessionKey),
		slog.String("chat_id", chatID),
		slog.String("message_id", messageID))

	return session, true
}

// GetSession retrieves an existing session by chatID and messageID.
//
// Parameters:
//   - chatID: Chat session identifier
//   - messageID: AI response message identifier
//
// Returns:
//   - *StreamSession: The session, or nil if not found
//
// Thread-safe: Uses read lock for efficient concurrent lookups.
func (sm *StreamManager) GetSession(chatID, messageID string) *StreamSession {
	sessionKey := sm.makeSessionKey(chatID, messageID)

	sm.mu.RLock()
	defer sm.mu.RUnlock()

	return sm.sessions[sessionKey]
}

// CleanupExpiredSessions removes completed sessions older than TTL.
//
// Parameters:
//   - ttl: Time-to-live for completed sessions (use sessionTTL for default)
//
// Returns:
//   - int: Number of sessions cleaned up
//
// Behavior:
//   - Only removes completed sessions
//   - In-progress sessions are never removed
//   - Under memory pressure, TTL is reduced automatically
//
// Thread-safe: Uses write lock during cleanup.
func (sm *StreamManager) CleanupExpiredSessions(ttl time.Duration) int {
	now := time.Now()
	cleaned := 0

	sm.mu.Lock()
	defer sm.mu.Unlock()

	for key, session := range sm.sessions {
		if !session.IsCompleted() {
			continue // Don't remove in-progress streams
		}

		// Calculate session age
		completedAt := session.completedAt
		if completedAt.IsZero() {
			// Session completed but timestamp not set (shouldn't happen)
			continue
		}

		age := now.Sub(completedAt)
		if age > ttl {
			// Save message to Firestore before cleanup
			sm.saveSessionMessage(session)

			// Remove from map
			delete(sm.sessions, key)
			cleaned++

			sm.logger.Debug("cleaned up expired session",
				slog.String("session_key", key),
				slog.Duration("age", age),
				slog.Duration("ttl", ttl))
		}
	}

	if cleaned > 0 {
		sm.logger.Info("cleaned up expired sessions",
			slog.Int("cleaned", cleaned),
			slog.Int("remaining", len(sm.sessions)))
	}

	// Update metrics
	sm.metricsLock.Lock()
	sm.totalSessionsCompleted += int64(cleaned)
	sm.metricsLock.Unlock()

	return cleaned
}

// saveSessionMessage extracts content from a completed session and saves to Firestore.
// This is called during session cleanup to ensure messages are always saved.
//
// Parameters:
//   - session: The completed session to save
//
// Note: This is a fallback mechanism. Ideally, message storage happens immediately
// after stream completion in the proxy handler.
func (sm *StreamManager) saveSessionMessage(session *StreamSession) {
	// Skip if no message service configured
	if sm.messageService == nil {
		return
	}

	// Extract content from chunks
	content := session.GetContent()
	if content == "" {
		// Empty content, nothing to save
		return
	}

	// TODO: PROXY-REFACTOR - We need userID and encryption settings here
	// For now, skip automatic saving during cleanup
	// Message saving should happen in the proxy handler right after completion
	sm.logger.Debug("session cleanup: message should have been saved by handler",
		slog.String("chat_id", session.chatID),
		slog.String("message_id", session.messageID),
		slog.Int("content_length", len(content)))
}

// GetActiveStreams returns a list of all active stream sessions.
// Used for observability and debugging.
//
// Returns:
//   - []StreamInfo: List of stream metadata
//
// Thread-safe: Uses read lock.
func (sm *StreamManager) GetActiveStreams() []StreamInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	infos := make([]StreamInfo, 0, len(sm.sessions))
	for _, session := range sm.sessions {
		infos = append(infos, session.GetInfo())
	}

	return infos
}

// GetActiveStreamForChat returns information about an active (not completed) stream for the given chat.
//
// Parameters:
//   - chatID: Chat session identifier
//
// Returns:
//   - *StreamInfo: Stream metadata, or nil if no active stream found
//
// Thread-safe: Uses read lock.
//
// Note: Only returns streams that are NOT completed. Completed streams are excluded
// because clients should not attempt to join them via the replay endpoint.
func (sm *StreamManager) GetActiveStreamForChat(chatID string) *StreamInfo {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	// Search for an active (not completed) session for this chat
	for key, session := range sm.sessions {
		// Check if session belongs to this chat
		if !strings.HasPrefix(key, chatID+":") {
			continue
		}

		// Only return if stream is NOT completed (active generation in progress)
		if !session.IsCompleted() {
			info := session.GetInfo()
			return &info
		}
	}

	return nil
}

// GetMetrics returns current streaming metrics.
// Used for monitoring and alerting.
//
// Returns:
//   - StreamMetrics: Current metrics
//
// Thread-safe: Uses read locks.
func (sm *StreamManager) GetMetrics() StreamMetrics {
	sm.mu.RLock()
	activeCount := 0
	completedCount := 0
	totalSubscribers := 0
	memoryBytes := int64(0)

	for _, session := range sm.sessions {
		if session.IsCompleted() {
			completedCount++
		} else {
			activeCount++
		}
		totalSubscribers += session.GetSubscriberCount()

		// Estimate memory usage (rough approximation)
		chunks := session.GetStoredChunks()
		for _, chunk := range chunks {
			memoryBytes += int64(len(chunk.Line))
		}
	}
	sm.mu.RUnlock()

	return StreamMetrics{
		ActiveStreams:    activeCount,
		TotalSubscribers: totalSubscribers,
		CompletedStreams: completedCount,
		MemoryUsageBytes: memoryBytes,
	}
}

// cleanupLoop runs periodically to clean up expired sessions.
// Runs in a background goroutine started by NewStreamManager().
func (sm *StreamManager) cleanupLoop() {
	defer sm.cleanupWg.Done()

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Check memory usage
			metrics := sm.GetMetrics()
			ttl := sessionTTL

			// Under memory pressure, reduce TTL to clean up more aggressively
			if metrics.MemoryUsageBytes > maxMemoryBytes {
				ttl = 1 * time.Minute
				sm.logger.Warn("memory pressure detected, reducing session TTL",
					slog.Int64("memory_bytes", metrics.MemoryUsageBytes),
					slog.Int64("max_bytes", maxMemoryBytes),
					slog.Duration("reduced_ttl", ttl))
			}

			// Run cleanup
			cleaned := sm.CleanupExpiredSessions(ttl)

			// Log metrics periodically
			if cleaned > 0 || metrics.ActiveStreams > 0 {
				sm.logger.Info("stream manager status",
					slog.Int("active_streams", metrics.ActiveStreams),
					slog.Int("completed_streams", metrics.CompletedStreams),
					slog.Int("total_subscribers", metrics.TotalSubscribers),
					slog.Int64("memory_bytes", metrics.MemoryUsageBytes),
					slog.Int("cleaned", cleaned))
			}

		case <-sm.shutdownCleanup:
			sm.logger.Info("stream manager cleanup loop stopped")
			return
		}
	}
}

// Shutdown gracefully shuts down the stream manager.
//
// Behavior:
//   - Stops cleanup goroutine
//   - Waits for cleanup to finish
//   - Does NOT wait for in-progress streams (handled by server shutdown)
//
// Call this during server shutdown to ensure clean exit.
func (sm *StreamManager) Shutdown() {
	sm.logger.Info("shutting down stream manager")

	// Stop cleanup loop
	close(sm.shutdownCleanup)
	sm.cleanupWg.Wait()

	// Log final stats
	sm.metricsLock.RLock()
	defer sm.metricsLock.RUnlock()

	sm.logger.Info("stream manager shutdown complete",
		slog.Int64("total_sessions_created", sm.totalSessionsCreated),
		slog.Int64("total_sessions_completed", sm.totalSessionsCompleted),
		slog.Int64("total_subscriptions", sm.totalSubscriptions))
}

// makeSessionKey creates a unique key for a session.
// Format: "chatID:messageID"
func (sm *StreamManager) makeSessionKey(chatID, messageID string) string {
	return fmt.Sprintf("%s:%s", chatID, messageID)
}

// RecordSubscription increments the subscription counter.
// Called when a new subscriber joins any session.
func (sm *StreamManager) RecordSubscription() {
	sm.metricsLock.Lock()
	defer sm.metricsLock.Unlock()
	sm.totalSubscriptions++
}

// SetToolExecutor sets the tool executor for tool call execution.
// This should be called during initialization, before any sessions are created.
func (sm *StreamManager) SetToolExecutor(executor *ToolExecutor) {
	sm.toolExecutor = executor
}

// SaveCompletedSession saves a completed session's message to Firestore.
//
// Parameters:
//   - session: The completed session
//   - userID: User ID for Firestore path
//   - encryptionEnabled: Whether to encrypt the message
//
// This should be called by the proxy handler immediately after stream completion.
//
// Returns:
//   - error: If save failed
func (sm *StreamManager) SaveCompletedSession(ctx context.Context, session *StreamSession, userID string, encryptionEnabled *bool) error {
	if sm.messageService == nil {
		return fmt.Errorf("message service not configured")
	}

	// Extract content
	content := session.GetContent()
	if content == "" {
		return fmt.Errorf("no content to save")
	}

	// Check if stopped
	stopped := session.IsStopped()
	// Get stop info
	stoppedBy, stopReason := session.GetStopInfo()

	// Log what we're saving
	sm.logger.Info("saving completed session to Firestore",
		slog.String("chat_id", session.chatID),
		slog.String("message_id", session.messageID),
		slog.Int("content_length", len(content)),
		slog.Bool("stopped", stopped),
		slog.String("stopped_by", stoppedBy))

	// Build message with stop metadata
	msg := messaging.MessageToStore{
		UserID:            userID,
		ChatID:            session.chatID,
		MessageID:         session.messageID,
		IsFromUser:        false, // AI response
		Content:           content,
		IsError:           session.GetError() != nil,
		EncryptionEnabled: encryptionEnabled,
		Stopped:           stopped,
		StoppedBy:         stoppedBy,
		StopReason:        string(stopReason),
	}

	// Store asynchronously
	return sm.messageService.StoreMessageAsync(ctx, msg)
}
