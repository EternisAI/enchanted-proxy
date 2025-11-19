# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Enchanted TEE Proxy** is a secure proxy service designed to run inside AWS Nitro Enclaves (Trusted Execution Environments) for the Enchanted Personal AI app. The proxy mediates API requests between the Enchanted mobile/desktop app and external AI services, providing authentication, rate limiting, usage tracking, and message storage while ensuring data privacy through hardware isolation.

The system supports multiple AI providers (OpenAI, OpenRouter, Near AI, Tinfoil, etc.), implements a freemium/Pro subscription model with usage quotas, and includes specialized features like Deep Research (WebSocket-based advanced research), message encryption, and Telegram bot integration.

## Development Commands

### Running the Server
```bash
make run                          # Start the development server (port 8080 by default)
go run cmd/server/main.go         # Alternative: run directly
```

### Building and Testing
```bash
make build                        # Build all packages
make test                         # Run tests with race detector
make lint                         # Format and lint code with golangci-lint
make deadcode                     # Check for dead functions
```

### Database Operations
```bash
make sqlc                         # Generate Go code from SQL queries (using sqlc)
```

To run database migrations, the server automatically applies pending migrations on startup using goose (see `internal/storage/pg/migrations.go`).

### Testing Individual Components
```bash
# Run tests for a specific package
go test ./internal/proxy -v -race

# Run a specific test
go test ./internal/auth -run TestFirebaseAuth -v
```

## Architecture Overview

### Core Components

1. **Main Server** (`cmd/server/main.go`)
   - Initializes two HTTP servers:
     - **REST API Server** (port 8080): Main proxy functionality, OAuth, MCP, subscriptions, deep research
     - **GraphQL Server** (port 8081): Telegram bot integration (optional, only if Telegram is enabled)
   - Sets up services, handlers, middleware, and graceful shutdown

2. **Authentication** (`internal/auth/`)
   - Supports two validator types (configured via `VALIDATOR_TYPE` env var):
     - `firebase`: Firebase Auth token validation (default)
     - `jwk`: JWT validation using JWKS URL
   - `FirebaseAuthMiddleware`: Gin middleware that requires valid authentication
   - `FirebaseClient`: Provides Firestore client access for usage tracking

3. **Proxy Service** (`internal/proxy/`)
   - Core functionality: proxies OpenAI-compatible API requests to various backends
   - Handles multiple endpoints: `/chat/completions`, `/embeddings`, `/audio/speech`, `/audio/transcriptions`, etc.
   - Selects API key based on `X-BASE-URL` header (see `allowedBaseURLs` in main.go)
   - Integrates with message storage and title generation services
   - Uses request tracking middleware for rate limiting and usage tracking

4. **Request Tracking & Rate Limiting** (`internal/request_tracking/`)
   - Worker pool-based async processing (configurable pool size and buffer)
   - Tracks token usage in PostgreSQL (`request_logs` table)
   - Implements freemium/Pro tier quotas:
     - **Free tier**: Limited lifetime tokens (default: 20,000)
     - **Drip tier**: Daily message limits (default: 10/day)
     - **Pro tier**: Daily token limits (default: 500,000/day)
   - Rate limiting modes: blocking (default) or log-only
   - Subscription validation via IAP (In-App Purchase) service

5. **Deep Research** (`internal/deepr/`)
   - WebSocket-based proxy to external deep research backend
   - Endpoints:
     - `POST /api/v1/deepresearch/start`: Starts a new research session
     - `GET /api/v1/deepresearch/ws`: WebSocket connection for streaming research
     - `POST /api/v1/deepresearch/clarify`: Submit clarification responses
   - Session persistence: stores messages to PostgreSQL for reconnection support
   - Freemium rate limiting: one free session per user, unlimited for Pro users
   - See `docs/deep-research.md` and `docs/deep-research-reconnection.md` for details

6. **Message Storage** (`internal/messaging/`)
   - Optional encrypted message storage to Firestore (controlled by `MESSAGE_STORAGE_ENABLED`)
   - Worker pool architecture for async Firestore writes
   - Supports AES-GCM encryption with keys exchanged via key-share service
   - Graceful degradation: falls back to plaintext if encryption fails (unless `MESSAGE_STORAGE_REQUIRE_ENCRYPTION=true`)
   - Integrated with title generation service for automatic chat titling

7. **Database Layer** (`internal/storage/pg/`)
   - PostgreSQL with connection pooling (configurable via env vars)
   - **sqlc** for type-safe SQL queries (queries in `internal/storage/pg/queries/`, generated code in `internal/storage/pg/sqlc/`)
   - **goose** for migrations (migrations in `internal/storage/pg/migrations/`)
   - Migrations run automatically on server startup
   - Key tables: `request_logs`, `entitlements`, `invite_codes`, `telegram_chats`, `deep_research_messages`, `tasks`

8. **GraphQL API** (`graph/`)
   - Uses **gqlgen** (schema: `schema.graphql`, resolvers: `graph/schema.resolvers.go`)
   - Telegram-specific API: query messages, send messages, subscribe to new messages
   - WebSocket subscriptions for real-time Telegram message delivery
   - CORS configured for allowed origins (default: localhost:3000)

### Service Integrations

- **OAuth** (`internal/oauth/`): Token exchange and refresh for Google, Slack, Twitter
- **Composio** (`internal/composio/`): Connected account management and token refresh
- **MCP (Model Context Protocol)** (`internal/mcp/`): Perplexity and Replicate tool integrations
- **Search** (`internal/search/`): SerpAPI and Exa AI search endpoints
- **IAP** (`internal/iap/`): Apple App Store subscription verification
- **Telegram** (`internal/telegram/`): Bot service with NATS pub/sub integration
- **Tasks** (`internal/task/`): Temporal workflow integration for async task processing
- **Key Sharing** (`internal/keyshare/`): End-to-end encryption key exchange via WebSocket sessions

### Configuration

All configuration is loaded from environment variables (with `.env` file support). See `internal/config/config.go` for the complete list. Key settings:

- **Server**: `PORT`, `GIN_MODE`
- **Database**: `DATABASE_URL`, `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`
- **Auth**: `VALIDATOR_TYPE`, `FIREBASE_PROJECT_ID`, `FIREBASE_CRED_JSON`, `JWT_JWKS_URL`
- **API Keys**: `OPENAI_API_KEY`, `OPENROUTER_MOBILE_API_KEY`, `OPENROUTER_DESKTOP_API_KEY`, etc.
- **Rate Limiting**: `RATE_LIMIT_ENABLED`, `RATE_LIMIT_LOG_ONLY`, `FREE_LIFETIME_TOKENS`, `PRO_DAILY_TOKENS`
- **Deep Research**: `DEEP_RESEARCH_RATE_LIMIT_ENABLED`
- **Message Storage**: `MESSAGE_STORAGE_ENABLED`, `MESSAGE_STORAGE_REQUIRE_ENCRYPTION`, worker pool settings
- **Logging**: `LOG_LEVEL` (debug/info/warn/error), `LOG_FORMAT` (text/json)
- **Temporal**: `TEMPORAL_ENDPOINT`, `TEMPORAL_NAMESPACE`, `TEMPORAL_API_KEY`
- **Telegram**: `ENABLE_TELEGRAM_SERVER`, `TELEGRAM_TOKEN`, `NATS_URL`

### Logging

Structured logging using `internal/logger/` (wraps `log/slog` with colored output via `lmittmann/tint`):
- Component-based logging: each service creates a logger with `.WithComponent("component_name")`
- Context-aware: request ID, user ID, chat ID automatically included via middleware
- Levels: debug, info, warn, error
- Format: text (pretty-printed with colors) or JSON

## Common Development Patterns

### Adding a New SQL Query

1. Add SQL to appropriate file in `internal/storage/pg/queries/` (e.g., `request_logs.sql`)
2. Run `make sqlc` to generate Go code
3. Use the generated methods from `db.Queries` (e.g., `db.Queries.CreateRequestLog(ctx, params)`)

### Adding a New Database Migration

1. Create new migration file in `internal/storage/pg/migrations/` with sequential number (e.g., `009_add_new_table.sql`)
2. Migrations auto-run on server startup
3. Use `-- +goose Up` and `-- +goose Down` directives for forward/backward migrations

### Adding a New REST Endpoint

1. Create handler in appropriate service (e.g., `internal/myservice/handlers.go`)
2. Register route in `setupRESTServer()` in `cmd/server/main.go`
3. Add authentication middleware if needed (already applied globally via `firebaseAuth.RequireAuth()`)
4. Consider adding request tracking middleware for rate limiting

### Adding a New GraphQL Type/Query/Mutation

1. Update `schema.graphql`
2. Run `go run github.com/99designs/gqlgen generate` to regenerate code
3. Implement resolver methods in `graph/schema.resolvers.go`

### Working with Firebase/Firestore

- **Auth**: Use `auth.FirebaseClient` for token validation
- **Firestore**: Access via `firebaseClient.GetFirestoreClient()`
- Collections: `deep_research_usage`, `messages/{chatId}/messages`, `encryption_keys/{sessionId}`

### Testing Strategy

- Unit tests: Place `*_test.go` files alongside code
- Use `go test ./... -race` to catch race conditions
- Mock external services (Firebase, external APIs) in tests
- Integration tests should use test database (set `DATABASE_URL` to test DB)

## Key Security Considerations

1. **TEE Environment**: This proxy is designed for AWS Nitro Enclaves - avoid logging sensitive data
2. **API Key Rotation**: API keys are loaded from env vars and mapped to base URLs in `allowedBaseURLs`
3. **Rate Limiting**: Always enabled in production to prevent abuse
4. **Message Encryption**: When `MESSAGE_STORAGE_ENABLED=true`, messages can be encrypted end-to-end via key-share service
5. **Authentication**: All endpoints (except `/wa` debug endpoint) require valid Firebase/JWT tokens
6. **CORS**: Configured for specific origins in production (see `CORS_ALLOWED_ORIGINS`)

## Troubleshooting

### Database Connection Issues
- Check `DATABASE_URL` format: `postgres://user:password@host:port/database?sslmode=disable`
- Verify connection pool settings match your database limits
- Check migrations ran successfully (logs on startup)

### Firebase Authentication Failures
- Ensure `FIREBASE_PROJECT_ID` and `FIREBASE_CRED_JSON` are set correctly
- For JWK validation, verify `JWT_JWKS_URL` is accessible
- Check token hasn't expired (tokens include expiration timestamps)

### Rate Limiting Not Working
- Verify `RATE_LIMIT_ENABLED=true`
- Check `RATE_LIMIT_LOG_ONLY` is not set to `true` in production
- Inspect `request_logs` table for usage data
- Verify `entitlements` table has correct subscription status

### Deep Research Connection Issues
- Check `DEEP_RESEARCH_WS` environment variable is set to backend host
- Verify WebSocket upgrade succeeds (check logs for upgrade errors)
- For reconnection issues, check `deep_research_messages` table for session persistence

### Message Storage Not Working
- Verify `MESSAGE_STORAGE_ENABLED=true`
- Check Firebase credentials are valid and Firestore API is enabled
- Inspect worker pool logs for Firestore write errors
- Check encryption key exchange logs if using E2EE
