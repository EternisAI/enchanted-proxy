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

1. **Authentication Layer** (pkg/auth/)
   - Firebase token validation via `firebase_validator.go:FirebaseTokenValidator`
   - JWT token validation via `jwt_validator.go:TokenValidator` 
   - Auth middleware in `middleware.go:RequireAuth()` validates Bearer tokens and injects user UUID into context

2. **Proxy Layer** (cmd/server/main.go)
   - `proxyHandler()` function handles `/chat/completions` and `/embeddings` endpoints
   - Validates `X-BASE-URL` header against allowedBaseURLs map
   - Injects appropriate API keys (OpenAI/OpenRouter) into forwarded requests
   - Uses `httputil.ReverseProxy` for request forwarding

3. **OAuth Feature** (pkg/oauth/)
   - Feature-based package with models, handlers, and service
   - Supports Google, Slack, Twitter OAuth flows
   - Token exchange and refresh functionality
   - Platform-specific response parsing (especially Slack's non-standard format)

4. **Composio Feature** (pkg/composio/)
   - Feature-based package with models, handlers, and service
   - Connected account management
   - Token refresh for third-party integrations

5. **Invite Code Feature** (pkg/invitecode/)
   - Feature-based package with models, handlers, and service
   - Invite code system with whitelist functionality
   - Database integration with PostgreSQL/GORM

6. **Database Layer** (pkg/config/database.go)
   - PostgreSQL with GORM
   - Auto-migration for invite code models

**Configuration:**
- Environment-based config in `pkg/config/config.go`
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
- `pkg/auth/middleware.go:RequireAuth()` - Token validation
- `pkg/config/config.go` - Environment configuration
- `pkg/oauth/handlers.go` - OAuth flow endpoints
- `pkg/oauth/service.go:ExchangeToken()` - OAuth token exchange logic
- `pkg/composio/service.go` - Composio API integration
- `pkg/invitecode/service.go` - Invite code management