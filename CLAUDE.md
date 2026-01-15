# CLAUDE.md - Enchanted Proxy Architecture

> ⚠️ **PUBLIC REPOSITORY** - This is a production backend, intentionally public for TEE verification. Never commit secrets, credentials, internal URLs, or infrastructure details. All secrets are injected via environment variables.

## Overview

Secure reverse proxy service for the Silo AI ecosystem, designed to run in AWS Nitro Enclaves (TEE). Mediates requests between iOS/Web clients and AI providers (OpenAI, OpenRouter, Tinfoil, NEAR, etc.), handling authentication, rate limiting, usage tracking, subscription tier enforcement, and end-to-end encryption (E2EE) for message storage.

**Key Properties:**
- **Zero-Knowledge Architecture**: Private keys never stored unencrypted; messages encrypted before proxy receives them
- **Multi-Provider Support**: OpenAI, OpenRouter, Tinfoil, NEAR AI, self-hosted providers
- **Subscription Tiers**: Free/Plus/Pro with token multipliers and feature gating
- **E2EE Message Storage**: AI responses encrypted to user's public key before Firestore storage
- **WebSocket Proxy**: Deep research, real-time streaming, task messages
- **Multiple Auth Methods**: Firebase, JWT/JWK, OAuth (Google, Slack, Twitter)
- **Payment Integration**: Stripe subscriptions, App Store IAP, Zcash payment support

## Commands

```bash
make run          # Start dev server (localhost:8080)
make build        # Build all packages
make test         # Run tests with race detector
make lint         # Format and lint
make sqlc         # Regenerate SQL code after changing queries
make clean        # Clean build artifacts
```

## High-Level Request Flow

```
Client (iOS/Web)
    │
    ├─ HTTP: POST /v1/chat/completions
    │         Authorization: Bearer {firebase_id_token}
    │         Body: {model, messages[], stream: true/false}
    │
    ▼ [Auth Middleware]
    ├─ Validate Firebase ID token or JWT/JWK
    ├─ Extract user_id from claims
    │
    ▼ [Rate Limiting & Quota Enforcement]
    ├─ Check user's subscription tier (Free/Plus/Pro)
    ├─ Apply tier token limits (monthly/weekly/daily)
    ├─ Check per-request rate limit bucket
    │
    ▼ [Model Routing]
    ├─ Look up model in routing config
    ├─ Resolve provider (OpenAI, OpenRouter, Tinfoil, etc.)
    ├─ Apply token multiplier for cost accounting
    │
    ▼ [Request Transformation]
    ├─ Forward to upstream provider
    ├─ Inject provider API key (from config)
    ├─ Handle streaming or buffered responses
    │
    ▼ [Token Usage & Storage]
    ├─ Parse tokens from response
    ├─ Deduct from user's quota
    ├─ Save plaintext user message to Firestore (async)
    ├─ Encrypt AI response with user's public key
    ├─ Store encrypted response in Firestore
    │
    ▼ Client receives response
    ├─ Decrypts with local private key
    ├─ Displays plaintext to user
```

## Project Structure

```
cmd/server/
  main.go                   # Entry point, server setup, route registration
  
internal/
  auth/
    firebase_client.go      # Firebase Admin SDK client
    firebase_validator.go   # Validate Firebase ID tokens
    jwt_validator.go        # Validate JWT/JWK tokens
    middleware.go           # Global auth middleware (applied to most routes)
    token.go                # Token extraction & claims handling
    
  config/
    config.go               # All environment variables (see Config struct)
    routing.go              # Model router config parsing
    
  proxy/
    handlers.go             # Main chat completions handler
    stream_helpers.go       # Streaming response handling
    message_utils.go        # Extract message content, token counting
    title_utils.go          # Auto-generate chat titles
    
  request_tracking/
    service.go              # Track user quota usage, log to DB
    quota.go                # Calculate remaining tokens for tier
    
  tiers/
    tiers.go                # Tier definitions (Free/Plus/Pro configs)
    
  routing/
    model_router.go         # Route models to providers, apply multipliers
    model_router_test.go    # Test vectors for routing logic
    
  deepr/
    handlers.go             # WebSocket proxy for deep research service
    
  messaging/
    service.go              # Firestore message storage
    encryption.go           # ECDH + AES-256-GCM encryption
    firestore.go            # Firestore query helpers
    models.go               # Message/encryption data models
    
  storage/pg/
    migrations/             # Goose migrations (auto-run on startup)
    queries/                # SQL files (edit these, then run make sqlc)
    sqlc/                   # Generated code (don't edit)
    
  iap/
    service.go              # Apple App Store receipt validation
    handler.go              # IAP webhook handlers
    
  stripe/
    service.go              # Stripe subscription handling
    handler.go              # Stripe webhook handlers
    
  oauth/
    google.go               # Google OAuth token exchange
    slack.go                # Slack OAuth
    twitter.go              # Twitter/X OAuth
    
  telegram/
    service.go              # Telegram bot integration
    
  deepr/                    # Deep research WebSocket proxy
  task/                     # Scheduled task execution
  search/                   # Web search integration
  notifications/            # Firebase Cloud Messaging
  composio/                 # Tool integration (web automation)
  mcp/                      # Model Context Protocol support
  streaming/                # WebSocket streaming for tasks
  zcash/                    # Zcash payment support
  
graph/                      # GraphQL schema and resolvers (gqlgen)
deploy/                     # Deployment configs
  enclaver.yaml             # AWS Nitro Enclave configuration
docs/                       # Feature documentation
```

## Comprehensive Architecture Detail

### 1. Authentication & Authorization

#### Firebase Token Validation

```go
// User authenticates with mobile/web app
// Firebase returns ID token signed with private key
// Client includes: Authorization: Bearer {id_token}

// Proxy validates:
1. Token signature (using Firebase public keys from JWKS endpoint)
2. Issuer matches Firebase project
3. Token not expired
4. Extract claims: user_id, email, etc.
```

**Files:**
- `internal/auth/firebase_client.go` - Manages Firebase Admin SDK
- `internal/auth/firebase_validator.go` - Validates ID tokens
- `internal/auth/middleware.go` - Global middleware applied to routes

#### JWT/JWK Validation

Alternative to Firebase for custom tokens:

```yaml
# config/config.go environment variables:
VALIDATOR_TYPE: "jwk"              # or "firebase"
JWT_JWKS_URL: "https://..."        # JWKS endpoint
```

Validates JWT tokens using public keys from JWKS endpoint. Used for server-to-server authentication.

#### OAuth Token Exchange

Three supported OAuth providers:

```
1. Google OAuth
   - User logs in via Google
   - Proxy exchanges authorization code for ID token
   - ID token used for authentication

2. Slack OAuth
   - User authorizes Slack workspace
   - Proxy stores access token for Slack API calls
   - Enables Slack bot integration

3. Twitter/X OAuth
   - User authorizes Twitter account
   - Access token stored for Twitter API
```

**Files:**
- `internal/oauth/google.go`
- `internal/oauth/slack.go`
- `internal/oauth/twitter.go`

#### Error Handling

Auth failures return:
```json
{
  "error": "invalid_token",
  "error_description": "Token expired or invalid signature"
}
```

**Unauth routes** (few exceptions without auth middleware):
- `/health` - Health check
- `/webhook/stripe` - Stripe webhooks (verify with signature)
- `/webhook/telegram` - Telegram webhooks
- `/wa` - WhatsApp integration

### 2. Rate Limiting & Quota Enforcement

#### Tier System Overview

Three subscription tiers with independent token quotas:

```
Free Tier:
  - 20,000 monthly plan tokens
  - Can use: DeepSeek R1, Llama 3.3 70B, GLM-4.6, Dolphin Mistral, Qwen3
  - 1 lifetime deep research run
  - NO fallback quota

Plus Tier:
  - 100,000 monthly plan tokens
  - Access to all models
  - 5 deep research runs/day

Pro Tier:
  - 500,000 daily plan tokens (resets daily at 00:00 UTC)
  - Full model access including GPT-5 Pro
  - 10 deep research runs/day
  - 50,000 daily fallback quota
```

**Token Multipliers** (cost adjustment per model):
```
Cheap models:     0.5× - 0.8×  (Dolphin Mistral, Qwen3, GLM-4.6)
Standard:         1×            (DeepSeek R1, Llama 70B)
Premium:          4× - 6×       (GPT-4.1, GPT-5)
Ultra-premium:    50×           (GPT-5 Pro - Responses API)
```

Plan tokens = Raw tokens × Multiplier

**Files:**
- `internal/tiers/tiers.go` - Tier definitions and feature configs
- `internal/request_tracking/service.go` - Quota tracking and enforcement
- `TIER_LIMITS.md` - Complete tier reference documentation

#### Quota Reset Periods

Tiers can have multiple active quotas (all enforced independently):

```
Monthly Reset:    1st of month, 00:00 UTC
Weekly Reset:     Every Monday, 00:00 UTC
Daily Reset:      Every day, 00:00 UTC
```

Example: Free tier has 20k monthly quota. Every request counts against it. On Feb 1st, resets to 20k.

#### Enforcement Workflow

```go
// For each request:
1. Extract user_id from auth token
2. Look up user's subscription tier
3. Get tier's quota configuration
4. Check all active quota periods:
   - Monthly: Has user exceeded monthly limit?
   - Weekly: Has user exceeded weekly limit?
   - Daily: Has user exceeded daily limit?
5. If any quota exceeded:
   - For Free tier: Return 429 (Too Many Requests)
   - For Plus/Pro with fallback: Switch to fallback model
6. After response, deduct tokens from all applicable quotas
```

**Database schema:**
```sql
-- internal/storage/pg/migrations/
users_plan_token_usage(
  user_id,
  period (month/week/day),
  tokens_used INT,
  reset_at TIMESTAMP
)
```

### 3. Model Routing & Token Multipliers

#### How Routing Works

```yaml
# Config from environment YAML:
model_router:
  providers:
    - name: "openai"
      api_key_env: "OPENAI_API_KEY"
      base_url: "https://api.openai.com/v1"
      
  models:
    - canonical_name: "gpt-5"
      display_name: "GPT-5 (Latest)"
      aliases: ["gpt-5", "gpt-5-latest"]
      provider: "openai"
      multiplier: 6.0
      tier_access: ["pro"]
      available: true
      
    - canonical_name: "gpt-5-pro"
      display_name: "GPT-5 Pro (Responses API)"
      aliases: ["gpt-5-pro"]
      provider: "openai"
      multiplier: 50.0  # 50× cost
      tier_access: ["pro"]
      available: true
      mode: "responses"  # Special handling for Responses API
```

**Client sends:**
```json
{
  "model": "gpt-5-pro",
  "messages": [...]
}
```

**Proxy routes to:**
```
1. Look up "gpt-5-pro" in model registry
2. Find canonical_name: "gpt-5-pro"
3. Get provider: "openai"
4. Get base_url: "https://api.openai.com/v1"
5. Get multiplier: 50.0
6. Check tier access: ["pro"] - user must be Pro tier
7. Verify available: true
8. Route request to OpenAI with canonical model name
9. Count tokens × 50.0 against quota
```

**Files:**
- `internal/routing/model_router.go` - Routing logic and multiplier application
- `internal/config/routing.go` - Configuration structure parsing
- `internal/routing/model_router_test.go` - Test vectors for routing

#### Adding a New Model

1. Edit environment YAML config or add to `internal/config/routing.go`:
```yaml
models:
  - canonical_name: "new-model"
    display_name: "New Model"
    aliases: ["new-model", "new-model-v1"]
    provider: "tinfoil"
    multiplier: 1.2
    tier_access: ["plus", "pro"]
    available: true
```

2. Redeploy (config loaded from environment)

#### Adding a New Provider

1. Add provider to config:
```yaml
providers:
  - name: "new-provider"
    api_key_env: "NEW_PROVIDER_API_KEY"
    base_url: "https://api.newprovider.com/v1"
```

2. Add models using that provider
3. Ensure API key is in environment (`internal/config/config.go`)

### 4. End-to-End Encryption (E2EE) Message Storage

#### How E2EE Works

```
User's Client (iOS/Web):
  1. Has passkey + account key (P-256 ECDH keypair)
  2. Private key is encrypted locally and never leaves device
  3. Public key is stored in Firestore

Proxy:
  1. Receives plaintext from user (client decrypts passkey before sending)
  2. Sends plaintext to AI provider
  3. Receives plaintext response from AI
  4. Encrypts response with user's public key using ECIES
  5. Stores encrypted response in Firestore

Client receives:
  1. Encrypted response from Firestore
  2. Decrypts with private key (unlocked by passkey)
  3. Displays plaintext to user
```

#### Encryption Algorithm (ECIES + HKDF + AES-256-GCM)

```
Message Encryption Flow:

1. Parse user's P-256 public key from JWK format
2. Generate ephemeral P-256 keypair
3. ECDH key agreement: ephemeralPriv × recipientPub → shared secret
4. HKDF-SHA256 derives message encryption key:
   - Input: shared secret
   - Salt: random 32 bytes
   - Info: "message-encryption"
   - Output: 32-byte AES-256-GCM key
5. AES-256-GCM encrypt message:
   - Key: derived key (32 bytes)
   - IV: random 12 bytes
   - AAD: (none)
   - Output: ciphertext + 16-byte auth tag
6. Package output: base64(ephemeralPubKey[65] || nonce[12] || ciphertext || tag[16])
```

**Critical Constants:**
```go
const (
  ACCOUNT_KEY_VERSION           = 1
  KEK_LENGTH                    = 32    // 256 bits
  AES_GCM_IV_LENGTH             = 12    // 96 bits
  AES_GCM_TAG_LENGTH            = 16    // 128 bits
  KEK_DERIVATION_INFO           = "silo-kek-v1"
  MESSAGE_ENCRYPTION_INFO       = "message-encryption"
  EPHEMERAL_PUBLIC_KEY_SIZE     = 65    // Uncompressed P-256 point
)
```

**Files:**
- `internal/messaging/encryption.go` - ECIES implementation
- `internal/messaging/service.go` - Message storage orchestration
- `internal/messaging/firestore.go` - Firestore queries
- Root `ENCRYPTION_*.md` files - Cross-platform compatibility analysis

### 5. Deep Research WebSocket Proxy

#### WebSocket Proxy Architecture

```
Client (iOS/Web)
  │
  ├─ WebSocket: GET /v1/deep-research?query=...
  │  Authorization: Bearer {id_token}
  │
  ▼ [Auth middleware]
  ├─ Validate token
  │
  ▼ [Deep Research Handler]
  ├─ Check tier has deep research enabled
  ├─ Check daily run limit not exceeded
  ├─ Increment active sessions counter
  │
  ▼ [Proxy WebSocket to Backend Service]
  ├─ silo_deep_research service listens on localhost:8001
  ├─ Proxy forwards client messages
  ├─ Proxy forwards service messages to client
  │
  ▼ [Cleanup on Close]
  ├─ Decrement active sessions
  ├─ Increment lifetime run counter
  ├─ Save usage metrics
```

**Features:**
- Real-time streaming progress updates
- Clarification pausing (agent asks user clarifying questions)
- Message replay on reconnect
- Token counting with GLM-4.6 multiplier (0.6×)
- Per-run token cap enforcement
- Max active sessions limit (prevents resource exhaustion)

**Files:**
- `internal/deepr/handlers.go` - WebSocket proxy logic
- `internal/request_tracking/service.go` - Deep research quota tracking

### 6. Subscription & Payment Integration

#### Stripe Subscriptions

```
Webhook Flow:
1. Stripe sends event: customer.subscription.created/updated/deleted
2. Proxy receives at `/webhook/stripe` (signature verified)
3. Query user by Stripe customer ID
4. Update user's tier in PostgreSQL
5. Cache tier in memory (TTL: 5 minutes)

Next request:
- Check cached tier first
- If cache miss, query DB
- Enforce tier limits
```

**Files:**
- `internal/stripe/service.go` - Stripe SDK integration
- `internal/stripe/handler.go` - Webhook processing
- `internal/storage/pg/queries/user_subscription.sql` - Queries

#### App Store IAP (In-App Purchases)

```
Client Flow:
1. User purchases subscription in App Store
2. Client gets receipt (JWT token)
3. Client sends receipt to `/v1/user/iap-verify` endpoint
4. Proxy validates receipt with Apple
5. Update user's tier
6. Return subscription status

Validation:
- Call Apple's App Attest verification
- Check bundle ID matches expected
- Verify expiration date
- Check receipt signature
```

**Files:**
- `internal/iap/service.go` - Apple verification
- `internal/iap/handler.go` - IAP endpoints

#### Zcash Payment

Cryptocurrency payment support for privacy-conscious users.

**Files:**
- `internal/zcash/` - Zcash integration

### 7. Firestore Message Storage Schema

```sql
-- Collection: chats/{chatID}
{
  id: string,
  user_id: string,
  created_at: timestamp,
  updated_at: timestamp,
  title: string,
  model: string,
  messages: [
    {
      id: string,
      role: "user" | "assistant",
      content: string (encrypted for assistant),
      tokens: number,
      created_at: timestamp
    }
  ]
}

-- User messages stored plaintext (no E2EE needed)
-- AI responses encrypted with user's public key
-- Encryption: base64(ephemeralPubKey || nonce || ciphertext || tag)
```

### 8. PostgreSQL Schema

```sql
-- Users
users(
  id UUID PRIMARY KEY,
  firebase_uid TEXT UNIQUE,
  email TEXT,
  tier VARCHAR(20) DEFAULT 'free',
  stripe_customer_id TEXT,
  created_at TIMESTAMP,
  updated_at TIMESTAMP
)

-- Subscription tracking
user_subscriptions(
  user_id UUID FOREIGN KEY,
  stripe_subscription_id TEXT,
  tier VARCHAR(20),
  started_at TIMESTAMP,
  ends_at TIMESTAMP,
  updated_at TIMESTAMP
)

-- Quota usage
user_plan_token_usage(
  user_id UUID FOREIGN KEY,
  period VARCHAR(10) ('month'|'week'|'day'),
  tokens_used INT,
  reset_at TIMESTAMP,
  updated_at TIMESTAMP
)

-- Request logging (for analytics)
api_requests(
  id UUID PRIMARY KEY,
  user_id UUID FOREIGN KEY,
  model VARCHAR(100),
  provider VARCHAR(50),
  tokens_used INT,
  multiplier FLOAT,
  plan_tokens_deducted INT,
  created_at TIMESTAMP
)

-- Deep research tracking
deep_research_runs(
  id UUID PRIMARY KEY,
  user_id UUID FOREIGN KEY,
  query TEXT,
  tokens_used INT,
  status VARCHAR(20),
  created_at TIMESTAMP
)
```

### 9. Configuration & Environment Variables

**Core:**
```bash
PORT=8080
GIN_MODE=release                          # or "debug"
FIREBASE_PROJECT_ID=eternis-ai
DATABASE_URL=postgres://...
```

**OAuth & Auth:**
```bash
GOOGLE_CLIENT_ID=...
GOOGLE_CLIENT_SECRET=...
SLACK_CLIENT_ID=...
SLACK_CLIENT_SECRET=...
TWITTER_CLIENT_ID=...
TWITTER_CLIENT_SECRET=...
VALIDATOR_TYPE=firebase                   # or "jwk"
JWT_JWKS_URL=https://...
FIREBASE_CRED_JSON={...}
```

**AI Providers:**
```bash
OPENAI_API_KEY=...
OPENROUTER_MOBILE_API_KEY=...
OPENROUTER_DESKTOP_API_KEY=...
TINFOIL_API_KEY=...
NEAR_API_KEY=...
ETERNIS_INFERENCE_API_KEY=...
SERP_API_KEY=...
EXA_API_KEY=...
```

**Payments:**
```bash
STRIPE_API_KEY=...
STRIPE_WEBHOOK_SECRET=...
```

See `internal/config/config.go` for complete list and defaults.

## Development Guide

### How to Add a New AI Provider

1. Add provider config to environment:
```yaml
providers:
  - name: "myprovider"
    api_key_env: "MYPROVIDER_API_KEY"
    base_url: "https://api.myprovider.com/v1"
```

2. Add config to `internal/config/config.go`:
```go
type Config struct {
  MyProviderAPIKey string
  // ...
}
```

3. Add models using that provider:
```yaml
models:
  - canonical_name: "mymodel"
    provider: "myprovider"
    multiplier: 1.5
    tier_access: ["plus", "pro"]
```

4. Proxy automatically routes based on config (no code changes needed)

### How to Add a New Model

1. Update config YAML:
```yaml
models:
  - canonical_name: "newmodel-v2"
    display_name: "New Model V2"
    aliases: ["newmodel-v2", "newmodel-latest"]
    provider: "openrouter"
    multiplier: 2.5
    tier_access: ["pro"]
    available: true
```

2. Deploy (config reloaded)
3. Update TIER_LIMITS.md documentation

### How to Add a New Subscription Tier

1. Edit `internal/tiers/tiers.go`:
```go
const (
  TierFree Tier = "free"
  TierPlus Tier = "plus"
  TierPro  Tier = "pro"
  TierElite Tier = "elite"  // NEW
)

var Configs = map[Tier]Config{
  // ... existing tiers ...
  TierElite: {
    Name:                        "elite",
    DisplayName:                 "Elite",
    MonthlyPlanTokens:           1_000_000,
    DeepResearchDailyRuns:       unlimited,
    DeepResearchTokenCap:        50_000,
    // ...
  },
}
```

2. Add Stripe product and update payment service
3. Update TIER_LIMITS.md

### How to Modify Rate Limits

**For a tier:**
```go
TierPro: {
  DailyPlanTokens: 1_000_000,  // Increased from 500k
}
```

**For a model multiplier:**
```yaml
models:
  - canonical_name: "gpt-5"
    multiplier: 8.0  # Increased from 6.0
```

**For deep research:**
```go
TierPro: {
  DeepResearchDailyRuns: 20,  // Increased from 10
  DeepResearchTokenCap:  20_000,  // Increased from 10k
}
```

### How to Add a New Endpoint

1. Create handler in relevant `internal/` package:
```go
// internal/myfeature/handlers.go
func (s *Service) HandleMyFeature(c *gin.Context) {
  userID := c.GetString("user_id")  // From auth middleware
  // ... implementation
  c.JSON(200, response)
}
```

2. Register in `cmd/server/main.go`:
```go
apiRoutes := r.Group("/v1", authMiddleware)
apiRoutes.POST("/my-feature", handlers.HandleMyFeature)
```

3. For auth-free endpoints, register in public group:
```go
publicRoutes := r.Group("/v1")
publicRoutes.POST("/public-endpoint", handlers.HandlePublic)
```

## Testing

### Unit Tests

```bash
# Test model routing
go test ./internal/routing -v

# Test tier limits
go test ./internal/tiers -v

# Run all tests
make test
```

### Integration Tests

E2EE message encryption cross-platform:
```bash
# Test vectors in test-vectors/encryption-compatibility.json
# Validates iOS ↔ Web ↔ Proxy compatibility
```

### Load Testing Tier Limits

```bash
# Verify rate limiting works under load
go test -bench=BenchmarkQuotaEnforcement ./internal/request_tracking
```

## Deployment & Security

### AWS Nitro Enclave (TEE)

Enclave config in `deploy/enclaver.yaml`:

```yaml
enclave:
  name: enchanted-proxy
  image: enchanted-proxy:latest
  memory: 4096  # MB
  cpus: 2
  
  # Egress allowlist (all other addresses denied)
  network:
    allowlist:
      - api.openai.com
      - api.openrouter.ai
      - inference.tinfoil.sh
      - firestore.googleapis.com
      - cloud-api.near.ai
```

**Security Benefits:**
- Code runs in AWS Nitro Enclave (isolated CPU)
- All outbound traffic restricted to allowlist
- No SSH access to enclave
- Encrypted attestation for verification
- Private keys never exposed (E2EE)

### Environment Variable Checklist

Before deployment:
- [ ] All API keys set (OPENAI_API_KEY, STRIPE_API_KEY, etc.)
- [ ] Firebase credentials loaded
- [ ] Database connection string valid
- [ ] Stripe webhook secret configured
- [ ] Model router config loaded and valid
- [ ] TEE allowlist updated if adding new provider

### Monitoring & Logging

Structured logging via `internal/logger/`:

```go
log := logger.WithContext(ctx).WithComponent("mycomponent")
log.Info("message", slog.String("key", "value"))
log.Error("error occurred", slog.String("error", err.Error()))
```

**Logs include:**
- Timestamp
- Log level
- Component (auth, proxy, deepr, etc.)
- User ID (if applicable)
- Request ID (for tracing)

Access logs and error tracking in observability platform.

## Key Patterns

**Database changes**: Add SQL to `internal/storage/pg/queries/`, run `make sqlc`, use generated methods.

**Migrations**: Add numbered file to `internal/storage/pg/migrations/` with `-- +goose Up/Down` directives. Auto-runs on startup.

**New endpoints**: Add handler in relevant `internal/` package, register in `cmd/server/main.go`.

**Tier/quota changes**: Edit `internal/tiers/tiers.go` for limits, environment config for multipliers.

**Config**: All env vars defined in `internal/config/config.go` with defaults.

## Notes

- Runs in Nitro Enclave (TEE) - egress restricted to allowlist in `deploy/enclaver.yaml`
- Structured logging via `internal/logger/` - use `.WithComponent()` for context
- Auth middleware applied globally in `cmd/server/main.go` (few exceptions like webhooks)
- E2EE encryption must be coordinated across iOS, Web, and Proxy (see root `ENCRYPTION_*.md`)
- All secrets injected via environment (never hardcoded)
- Public repository for TEE verification - no secrets in code
