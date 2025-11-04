# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Development Commands

**Build & Run:**
- `make run` - Run the server directly with Go
- `make build` - Build all Go packages 
- `go run cmd/server/main.go` - Direct server execution

**Code Quality:**
- `make lint` - Format and fix Go code with golangci-lint
- `make test` - Run all tests with race detection (`go test ./... -race`)

**Docker:**
- Multi-stage Docker build with Go 1.24 and Alpine runtime
- Exposes port 8080 by default
- Uses Railway deployment configuration

## Architecture Overview

**Core Service:** OAuth proxy server that validates Firebase/JWT tokens and forwards API requests to OpenAI/OpenRouter endpoints.

**Key Components:**

1. **Authentication Layer** (internal/auth/)
   - Firebase token validation via `firebase_validator.go:FirebaseTokenValidator`
   - JWT token validation via `jwt_validator.go:TokenValidator` 
   - Auth middleware in `middleware.go:RequireAuth()` validates Bearer tokens and injects user UUID into context

2. **Proxy Layer** (cmd/server/main.go)
   - `proxyHandler()` function handles `/chat/completions` and `/embeddings` endpoints
   - Validates `X-BASE-URL` header against allowedBaseURLs map
   - Injects appropriate API keys (OpenAI/OpenRouter) into forwarded requests
   - Uses `httputil.ReverseProxy` for request forwarding

3. **OAuth Feature** (internal/oauth/)
   - Feature-based package with models, handlers, and service
   - Supports Google, Slack, Twitter OAuth flows
   - Token exchange and refresh functionality
   - Platform-specific response parsing (especially Slack's non-standard format)

4. **Composio Feature** (internal/composio/)
   - Feature-based package with models, handlers, and service
   - Connected account management
   - Token refresh for third-party integrations

5. **Invite Code Feature** (internal/invitecode/)
   - Feature-based package with models, handlers, and service
   - Invite code system with whitelist functionality
   - Database integration with PostgreSQL/GORM

6. **Database Layer** (internal/config/database.go)
   - PostgreSQL with GORM
   - Auto-migration for invite code models

**Configuration:**
- Environment-based config in `internal/config/config.go`
- Supports both Firebase and JWT validation modes via `VALIDATOR_TYPE`
- Database connection via `DATABASE_URL`

**Request Flow:**
1. Client sends request with Authorization Bearer token + X-BASE-URL header
2. Firebase/JWT middleware validates token, extracts user UUID
3. Proxy handler validates base URL against allowed list
4. Request forwarded to target API with injected API key
5. Response returned to client

**Key Files:**
- `cmd/server/main.go:proxyHandler()` - Main proxy logic
- `internal/auth/middleware.go:RequireAuth()` - Token validation
- `internal/config/config.go` - Environment configuration
- `internal/oauth/handlers.go` - OAuth flow endpoints
- `internal/oauth/service.go:ExchangeToken()` - OAuth token exchange logic
- `internal/composio/service.go` - Composio API integration
- `internal/invitecode/service.go` - Invite code management

## End-to-End Encryption Support

### Overview

The proxy server **actively encrypts AI responses** before storing them in Firestore. This is an **AI chat application** (user ↔ AI), not peer-to-peer messaging.

**Proxy's Role:**
- Decrypts user messages to send to OpenAI (necessary for AI processing)
- Encrypts AI responses with user's public key before Firestore storage
- Fetches user public keys directly from Firestore (no caching - always uses latest key)
- Supports graceful degradation (plaintext fallback if encryption fails)

**Security:** Proxy never stores or has access to user private keys (only public keys).

### Implementation Architecture

**Key Components:**

| File | Purpose | Lines |
|------|---------|-------|
| `internal/messaging/encryption.go` | ECIES encryption/decryption | 141 |
| `internal/messaging/service.go` | Async message storage with encryption | 350+ |
| `internal/messaging/cache.go` | LRU cache for user public keys | 150+ |
| `internal/messaging/firestore.go` | Firestore read/write operations | 200+ |

### Message Storage Flow

**When AI Responds:**

```go
// In service.go:handleMessage()
func (s *Service) handleMessage(msg MessageToStore) {
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // 1. Fetch user's public key (with caching)
    publicKey, err := s.getPublicKey(ctx, msg.UserID)
    if err != nil {
        logger.Error("Failed to fetch public key", "error", err)
        // Graceful degradation: store as plaintext
        publicKey = nil
    }

    // 2. Encrypt message content (if public key available)
    var encryptedContent string
    var publicKeyUsed string
    if publicKey != nil && publicKey.Public != "" {
        encrypted, err := s.encryptionService.EncryptMessage(msg.Content, publicKey.Public)
        if err != nil {
            logger.Error("Encryption failed", "error", err)
            // Fall back to plaintext
            encryptedContent = msg.Content
            publicKeyUsed = "none"
        } else {
            encryptedContent = encrypted
            publicKeyUsed = publicKey.Public
        }
    } else {
        // No public key, store as plaintext
        encryptedContent = msg.Content
        publicKeyUsed = "none"
    }

    // 3. Save to Firestore
    chatMsg := &ChatMessage{
        ID:                  msg.MessageID,
        EncryptedContent:    encryptedContent,
        PublicEncryptionKey: publicKeyUsed,
        IsFromUser:          false, // AI response
        ChatID:              msg.ChatID,
        IsError:             msg.IsError,
        Timestamp:           firestore.ServerTimestamp,
    }
    err = s.firestoreClient.SaveMessage(ctx, msg.UserID, chatMsg)
    if err != nil {
        logger.Error("Failed to save message", "error", err)
    }
}
```

### Async Message Storage Architecture

**Worker Pool Design:**

```go
type Service struct {
    messageQueue      chan MessageToStore  // Buffered channel (10,000 capacity)
    workers           int                  // Default: 10 concurrent workers
    encryptionService *encryption.Service  // ECIES encryption
    firestoreClient   *firestore.Client
    publicKeyCache    *PublicKeyCache      // LRU cache (1000 entries, 15min TTL)
    shutdown          chan struct{}
    wg                sync.WaitGroup
}

// Start worker pool
func (s *Service) Start() {
    for i := 0; i < s.workers; i++ {
        s.wg.Add(1)
        go s.worker()
    }
}

// Worker processes messages from queue
func (s *Service) worker() {
    defer s.wg.Done()
    for msg := range s.messageQueue {
        s.handleMessage(msg)
    }
}
```

**Graceful Shutdown:**
- Drains remaining messages from queue before exit
- 10-second timeout per message
- Ensures no messages lost during deployment

### Public Key Caching

**File:** `internal/messaging/cache.go`

**LRU Cache Configuration:**

```go
type PublicKeyCache struct {
    maxSize int            // Default: 1000 entries
    ttl     time.Duration  // Default: 15 minutes
    cache   map[string]*CachedPublicKey
    mu      sync.RWMutex
}

type CachedPublicKey struct {
    UserID    string
    Public    string  // JWK JSON string
    ExpiresAt time.Time
}
```

**Cache Strategy:**
- Fetches from Firestore on cache miss
- Reduces Firestore reads by 95%+ during active usage
- Thread-safe with RWMutex
- Automatic expiration (15min TTL)

### Encryption Algorithm (ECIES)

**File:** `internal/messaging/encryption.go`

**Key Functions:**

- `EncryptMessage(content string, publicKeyJWK string) (string, error)` - ECIES encryption matching iOS/Web
- `parseJWKPublicKey(jwkJSON string) (*ecdh.PublicKey, error)` - JWK parsing with P-256 validation
- `ValidatePublicKey(publicKeyJWK string) error` - Public key validation

**Algorithm Details:**

```go
// ECIES encryption flow (matching iOS/Web):
// 1. Parse user's P-256 public key from JWK
// 2. Generate ephemeral P-256 keypair
// 3. ECDH: sharedSecret = ephemeralPrivate × userPublic
// 4. HKDF-SHA256: messageKey = HKDF(sharedSecret, "", "message-encryption")
// 5. AES-256-GCM with random 12-byte nonce
// 6. Output: base64(ephemeralPublicKey[65] || nonce[12] || ciphertext || tag[16])
```

**Dependencies:**

- `crypto/ecdh` - ECDH key agreement (P-256 curve)
- `crypto/aes` - AES-256-GCM encryption
- `golang.org/x/crypto/hkdf` - HKDF key derivation
- `crypto/sha256` - SHA-256 hashing

### JWK Format Support

**Supported JWK Structure:**

```json
{
  "kty": "EC",
  "crv": "P-256",
  "x": "base64url(X coordinate, 32 bytes)",
  "y": "base64url(Y coordinate, 32 bytes)"
}
```

**Validation:**

- Verifies `kty == "EC"` and `crv == "P-256"`
- Decodes base64url coordinates
- Validates point is on P-256 curve
- Returns `*ecdh.PublicKey` for encryption operations

### Cross-Platform Compatibility

**Critical Constants (must match iOS/Web):**

```go
const (
    messageEncryptionInfo = "message-encryption"  // HKDF info string
    ephemeralPublicKeySize = 65  // Uncompressed P-256 point (0x04 || X || Y)
    nonceSize = 12               // AES-GCM nonce
    tagSize = 16                 // AES-GCM authentication tag
)
```

**Binary Format:**

```
base64(ephemeralPublicKey[65] || nonce[12] || ciphertext || tag[16])
```

This format is **identical** to iOS and Web implementations, ensuring cross-platform encryption compatibility.

### Configuration

**Environment Variables:**

```bash
# Encryption behavior
MESSAGE_STORAGE_REQUIRE_ENCRYPTION=false  # If true, reject plaintext storage

# Worker pool
MESSAGE_WORKER_COUNT=10                    # Number of concurrent workers
MESSAGE_QUEUE_SIZE=10000                   # Message queue buffer size
```

### Graceful Degradation

**When Encryption Fails:**

1. User has no public key setup → Store as plaintext (`publicEncryptionKey: "none"`)
2. Public key fetch fails → Store as plaintext + log error
3. Encryption operation fails → Store as plaintext + log error
4. Strict mode (`MESSAGE_STORAGE_REQUIRE_ENCRYPTION=true`) → Reject request with error

### Security Properties

✅ **Strengths:**
- **Forward Secrecy:** Ephemeral keys generated per message
- **Authentication:** AES-GCM provides built-in authentication (tag)
- **Key Agreement:** ECDH with P-256 (256-bit security level)
- **Key Derivation:** HKDF-SHA256 for message key derivation
- **Zero Knowledge:** Proxy never stores or has access to user private keys

⚠️ **Limitations:**
- **Temporary Plaintext Access:** Proxy has momentary access to AI responses before encryption
- **Graceful Degradation:** Messages can be stored as plaintext if encryption fails (unless strict mode)
- **Cache Breach:** Public key cache could reveal which users are active (mitigated by 15min TTL)

### Testing

```bash
# Run encryption tests
go test ./internal/messaging/... -v

# Run all tests with race detection
make test

# Test encryption compatibility
go test ./internal/messaging/encryption_test.go -v
```

### Monitoring & Debugging

**Logging:**

```go
logger.Info("Encrypting AI response", "userId", userID, "messageId", messageID)
logger.Error("Encryption failed", "error", err, "userId", userID)
logger.Debug("Public key cache hit", "userId", userID, "hitRate", hitRate)
```

**Metrics to Monitor:**
- Message queue depth (alert if > 8000)
- Encryption success/failure rate
- Firestore read latency for public key fetches
- Average message processing time (should be < 500ms)

### Future Enhancements

- Message re-encryption for key rotation
- Per-chat encryption keys
- Encrypted webhook delivery
- Server-side search on encrypted messages (homomorphic encryption)