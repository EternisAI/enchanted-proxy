# CLAUDE.md

> ⚠️ **PUBLIC REPOSITORY** - This is a production backend, intentionally public for TEE verification. Never commit secrets, credentials, internal URLs, or infrastructure details. All secrets are injected via environment variables.

## Overview

Secure proxy service for the Eternis AI app, designed to run in AWS Nitro Enclaves. Mediates requests between mobile/desktop clients and AI providers, handling auth, rate limiting, and usage tracking.

## Commands

```bash
make run          # Start dev server
make build        # Build
make test         # Run tests with race detector
make lint         # Format and lint
make sqlc         # Regenerate SQL code after changing queries
```

## Project Structure

```
cmd/server/           # Entry point, server setup, route registration
internal/
  auth/               # Firebase/JWT authentication middleware
  config/             # Environment config (see config.go for all env vars)
  proxy/              # Core proxy handlers for AI API requests
  request_tracking/   # Rate limiting, usage tracking, tier enforcement
  tiers/              # Subscription tier definitions and limits
  routing/            # Model routing and token multipliers
  deepr/              # Deep research WebSocket proxy
  messaging/          # Encrypted message storage (Firestore)
  storage/pg/         # PostgreSQL layer
    queries/          # SQL files (edit these, then run make sqlc)
    migrations/       # Goose migrations (auto-run on startup)
    sqlc/             # Generated code (don't edit)
  iap/                # App Store subscription verification
  stripe/             # Stripe subscription handling
  oauth/              # OAuth token exchange
  telegram/           # Telegram bot integration
graph/                # GraphQL schema and resolvers (gqlgen)
deploy/               # Deployment configs (enclaver.yaml for TEE)
docs/                 # Feature documentation
```

## Key Patterns

**Database changes**: Add SQL to `internal/storage/pg/queries/`, run `make sqlc`, use generated methods.

**Migrations**: Add numbered file to `internal/storage/pg/migrations/` with `-- +goose Up/Down` directives. Auto-runs on startup.

**New endpoints**: Add handler in relevant `internal/` package, register in `cmd/server/main.go`.

**Tier/quota changes**: Edit `internal/tiers/tiers.go` for limits, `internal/routing/model_router.go` for token multipliers.

**Config**: All env vars defined in `internal/config/config.go` with defaults.

## Notes

- Runs in Nitro Enclave (TEE) - egress restricted to allowlist in `deploy/enclaver.yaml`
- Structured logging via `internal/logger/` - use `.WithComponent()` for context
- Auth middleware applied globally in `cmd/server/main.go` (few exceptions like webhooks)
