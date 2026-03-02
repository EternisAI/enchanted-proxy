package proxy

import (
	"context"
	"fmt"
	"sync"
)

// CancelRegistry is a lightweight map of chatID:messageID → cancel function.
// Used by simple streaming to support the stop endpoint without the full
// StreamManager/StreamSession machinery.
type CancelRegistry struct {
	mu      sync.RWMutex
	cancels map[string]context.CancelFunc
}

// NewCancelRegistry creates a new cancel registry.
func NewCancelRegistry() *CancelRegistry {
	return &CancelRegistry{
		cancels: make(map[string]context.CancelFunc),
	}
}

// Register stores a cancel function for the given chat/message pair.
func (r *CancelRegistry) Register(chatID, messageID string, cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[makeKey(chatID, messageID)] = cancel
}

// Cancel cancels the stream for the given chat/message pair.
// Returns an error if the stream is not found.
func (r *CancelRegistry) Cancel(chatID, messageID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := makeKey(chatID, messageID)
	cancel, ok := r.cancels[key]
	if !ok {
		return fmt.Errorf("stream not found: %s", key)
	}
	cancel()
	delete(r.cancels, key)
	return nil
}

// Remove removes a cancel function (called when stream completes naturally).
func (r *CancelRegistry) Remove(chatID, messageID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, makeKey(chatID, messageID))
}

func makeKey(chatID, messageID string) string {
	return fmt.Sprintf("%s:%s", chatID, messageID)
}
