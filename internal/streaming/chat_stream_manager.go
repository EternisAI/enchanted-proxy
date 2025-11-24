package streaming

import (
	"log/slog"
	"sync"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

const (
	// hubCleanupInterval is how often to check for idle hubs
	hubCleanupInterval = 5 * time.Minute

	// hubIdleTimeout is how long to keep a hub alive with no subscribers
	hubIdleTimeout = 2 * time.Minute
)

// ChatStreamManager manages all chat stream hubs.
// It creates hubs on-demand when clients connect and cleans them up when idle.
type ChatStreamManager struct {
	hubs          map[string]*hubEntry
	mu            sync.RWMutex
	streamManager *StreamManager
	logger        *logger.Logger

	// Cleanup
	shutdownCleanup chan struct{}
	cleanupWg       sync.WaitGroup
}

// hubEntry tracks a hub and its last activity time.
type hubEntry struct {
	hub          *ChatStreamHub
	lastActivity time.Time
}

// NewChatStreamManager creates a new chat stream manager.
func NewChatStreamManager(streamManager *StreamManager, logger *logger.Logger) *ChatStreamManager {
	csm := &ChatStreamManager{
		hubs:            make(map[string]*hubEntry),
		streamManager:   streamManager,
		logger:          logger.WithComponent("chat-stream-manager"),
		shutdownCleanup: make(chan struct{}),
	}

	// Start cleanup goroutine
	csm.cleanupWg.Add(1)
	go csm.cleanupLoop()

	logger.Info("chat stream manager initialized")

	return csm
}

// GetOrCreateHub gets or creates a hub for a chat using double-check locking pattern.
func (csm *ChatStreamManager) GetOrCreateHub(chatID string) *ChatStreamHub {
	csm.mu.RLock()
	if entry, exists := csm.hubs[chatID]; exists {
		entry.lastActivity = time.Now()
		csm.mu.RUnlock()
		return entry.hub
	}
	csm.mu.RUnlock()

	csm.mu.Lock()
	defer csm.mu.Unlock()

	if entry, exists := csm.hubs[chatID]; exists {
		entry.lastActivity = time.Now()
		return entry.hub
	}

	hub := NewChatStreamHub(chatID, csm.streamManager, csm.logger)
	csm.hubs[chatID] = &hubEntry{
		hub:          hub,
		lastActivity: time.Now(),
	}

	csm.logger.Info("created new chat hub",
		slog.String("chat_id", chatID),
		slog.Int("total_hubs", len(csm.hubs)))

	return hub
}

// GetHub gets an existing hub for a chat, returning nil if not found.
func (csm *ChatStreamManager) GetHub(chatID string) *ChatStreamHub {
	csm.mu.RLock()
	defer csm.mu.RUnlock()

	if entry, exists := csm.hubs[chatID]; exists {
		return entry.hub
	}

	return nil
}

// NotifyStreamStarted notifies the hub that a new stream has started.
// Only notifies if the hub exists (i.e., has active subscribers).
func (csm *ChatStreamManager) NotifyStreamStarted(chatID, messageID string, session *StreamSession) {
	hub := csm.GetHub(chatID)
	if hub == nil {
		return
	}

	hub.OnStreamStarted(messageID, session)
}

// CleanupIdleHubs removes hubs with no subscribers that have been idle for over 2 minutes.
func (csm *ChatStreamManager) CleanupIdleHubs() int {
	now := time.Now()
	cleaned := 0

	csm.mu.Lock()
	defer csm.mu.Unlock()

	for chatID, entry := range csm.hubs {
		if entry.hub.GetSubscriberCount() == 0 {
			idleTime := now.Sub(entry.lastActivity)
			if idleTime > hubIdleTimeout {
				entry.hub.Close()
				delete(csm.hubs, chatID)
				cleaned++

				csm.logger.Debug("cleaned up idle hub",
					slog.String("chat_id", chatID),
					slog.Duration("idle_time", idleTime))
			}
		} else {
			entry.lastActivity = now
		}
	}

	if cleaned > 0 {
		csm.logger.Info("cleaned up idle hubs",
			slog.Int("cleaned", cleaned),
			slog.Int("remaining", len(csm.hubs)))
	}

	return cleaned
}

// cleanupLoop runs periodically to clean up idle hubs.
func (csm *ChatStreamManager) cleanupLoop() {
	defer csm.cleanupWg.Done()

	ticker := time.NewTicker(hubCleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cleaned := csm.CleanupIdleHubs()

			// Log metrics
			csm.mu.RLock()
			totalHubs := len(csm.hubs)
			totalSubscribers := 0
			for _, entry := range csm.hubs {
				totalSubscribers += entry.hub.GetSubscriberCount()
			}
			csm.mu.RUnlock()

			if cleaned > 0 || totalHubs > 0 {
				csm.logger.Info("chat stream manager status",
					slog.Int("total_hubs", totalHubs),
					slog.Int("total_subscribers", totalSubscribers),
					slog.Int("cleaned", cleaned))
			}

		case <-csm.shutdownCleanup:
			csm.logger.Info("chat stream manager cleanup loop stopped")
			return
		}
	}
}

// Shutdown gracefully shuts down all hubs and stops the cleanup loop.
func (csm *ChatStreamManager) Shutdown() {
	csm.logger.Info("shutting down chat stream manager")

	close(csm.shutdownCleanup)
	csm.cleanupWg.Wait()

	csm.mu.Lock()
	for chatID, entry := range csm.hubs {
		entry.hub.Close()
		delete(csm.hubs, chatID)
	}
	csm.mu.Unlock()

	csm.logger.Info("chat stream manager shutdown complete")
}

// GetMetrics returns current chat stream metrics.
func (csm *ChatStreamManager) GetMetrics() ChatStreamMetrics {
	csm.mu.RLock()
	defer csm.mu.RUnlock()

	totalHubs := len(csm.hubs)
	totalSubscribers := 0

	for _, entry := range csm.hubs {
		totalSubscribers += entry.hub.GetSubscriberCount()
	}

	return ChatStreamMetrics{
		ActiveHubs:       totalHubs,
		TotalSubscribers: totalSubscribers,
	}
}

// ChatStreamMetrics provides aggregated metrics for chat streams.
type ChatStreamMetrics struct {
	ActiveHubs       int `json:"active_hubs"`
	TotalSubscribers int `json:"total_subscribers"`
}
