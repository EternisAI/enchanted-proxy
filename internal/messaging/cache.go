package messaging

import (
	"sync"
	"time"
)

// PublicKeyCache is a simple in-memory LRU cache for public keys
type PublicKeyCache struct {
	cache   map[string]*cacheEntry
	mu      sync.RWMutex
	maxSize int
	ttl     time.Duration
}

type cacheEntry struct {
	key       *UserPublicKey
	expiresAt time.Time
}

// NewPublicKeyCache creates a new cache
func NewPublicKeyCache(maxSize int, ttl time.Duration) *PublicKeyCache {
	return &PublicKeyCache{
		cache:   make(map[string]*cacheEntry),
		maxSize: maxSize,
		ttl:     ttl,
	}
}

// Get retrieves a key from cache
func (c *PublicKeyCache) Get(userID string) *UserPublicKey {
	c.mu.RLock()
	defer c.mu.RUnlock()

	entry, exists := c.cache[userID]
	if !exists {
		return nil
	}

	if time.Now().After(entry.expiresAt) {
		return nil
	}

	return entry.key
}

// Set stores a key in cache
func (c *PublicKeyCache) Set(userID string, key *UserPublicKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Simple eviction: remove oldest if at capacity
	if len(c.cache) >= c.maxSize {
		// Remove first entry (not truly LRU, but simple)
		for k := range c.cache {
			delete(c.cache, k)
			break
		}
	}

	c.cache[userID] = &cacheEntry{
		key:       key,
		expiresAt: time.Now().Add(c.ttl),
	}
}

// Clear removes all entries from cache
func (c *PublicKeyCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cache = make(map[string]*cacheEntry)
}

// Size returns the current number of entries in cache
func (c *PublicKeyCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.cache)
}
