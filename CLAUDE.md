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

The proxy server includes encryption utilities in `internal/messaging/encryption.go` to support future server-side encryption features and validation. The proxy **never has access to user private keys** - it only provides optional encryption utilities.

### Implementation

**File:** `internal/messaging/encryption.go` (141 lines)

**Key Functions:**

- `EncryptMessage(content string, publicKeyJWK string) (string, error)` - ECIES encryption matching iOS/Web
- `parseJWKPublicKey(jwkJSON string) (*ecdh.PublicKey, error)` - JWK parsing with P-256 validation
- `ValidatePublicKey(publicKeyJWK string) error` - Public key validation

**Algorithm Details:**

```go
// ECIES encryption flow (matching iOS/Web):
// 1. Parse recipient's P-256 public key from JWK
// 2. Generate ephemeral P-256 keypair
// 3. ECDH: sharedSecret = ephemeralPrivate × recipientPublic
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
    messageEncryptionInfo = "message-encryption"
    ephemeralPublicKeySize = 65  // Uncompressed P-256 point
    nonceSize = 12               // AES-GCM nonce
    tagSize = 16                 // AES-GCM authentication tag
)
```

**Binary Format:**

```
base64(ephemeralPublicKey[65] || nonce[12] || ciphertext || tag[16])
```

This format is **identical** to iOS and Web implementations, ensuring messages encrypted by the proxy can be decrypted by clients and vice versa.

### Security Properties

- **Forward Secrecy:** Ephemeral keys generated per message
- **Authentication:** AES-GCM provides built-in authentication (tag)
- **Key Agreement:** ECDH with P-256 (256-bit security level)
- **Key Derivation:** HKDF-SHA256 for message key derivation
- **Zero Knowledge:** Proxy never stores or has access to user private keys

### Use Cases

1. **Public Key Validation:** Verify user-provided public keys are well-formed
2. **Server-Side Encryption:** Encrypt messages on behalf of authenticated users (future feature)
3. **Testing & Verification:** Validate cross-platform encryption compatibility
4. **Key Exchange Support:** Facilitate secure key exchange flows

### Testing

```bash
# Run encryption tests
go test ./internal/messaging/... -v

# Run all tests with race detection
make test
```

### Future Enhancements

- Encrypted message routing between users
- Server-side re-encryption for key rotation
- Encrypted webhook delivery
- Secure message queuing