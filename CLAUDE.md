# CLAUDE.md

> ⚠️ **PUBLIC REPOSITORY** - Production backend, intentionally public for TEE verification. Never commit secrets, credentials, internal URLs, or infrastructure details.

## Overview

Secure proxy for Silo AI app, running in AWS Nitro Enclaves. Routes requests between clients and AI providers (OpenAI, OpenRouter, Tinfoil, NEAR AI, self-hosted). Handles auth, rate limiting, usage tracking, and encrypted message storage.

## Commands

```bash
make run          # Start dev server (requires config.yaml)
make build        # Build all packages
make test         # Run tests with race detector
make lint         # Format and lint (golangci-lint)
make sqlc         # Regenerate SQL code after changing queries
make deadcode     # Check for transitively dead functions
```

## Project Structure

```
cmd/
  server/             # Main entry point (main.go has all route registration)
  invite-generator/   # CLI tool for generating invite codes
config/
  config.yaml         # Model routing configuration (required at runtime)
internal/
  auth/               # Firebase/JWT token validation middleware
  background/         # Background polling for GPT-5 Pro slow responses
  config/             # Environment config (config.go has all env vars with defaults)
  deepr/              # Deep research WebSocket proxy
  fallback/           # Model routing fallback based on Prometheus metrics
  iap/                # App Store subscription verification
  invitecode/         # Invite code management
  keyshare/           # E2EE key sharing via WebSocket (device-to-device)
  logger/             # Structured logging (use .WithComponent() for context)
  mcp/                # MCP protocol support (Perplexity, Replicate integrations)
  messaging/          # Encrypted message storage to Firestore
  notifications/      # FCM push notifications
  oauth/              # OAuth token exchange (Google, Slack, Twitter)
  problem_reports/    # Problem reporting (creates Linear issues)
  proxy/              # Core proxy handlers for AI API requests
  request_tracking/   # Rate limiting, usage tracking, tier enforcement
  routing/            # Model router (auto-routes models to providers)
  search/             # Search via Exa AI and SerpAPI
  storage/pg/         # PostgreSQL layer
    queries/          # SQL files (edit these, then run make sqlc)
    migrations/       # Goose migrations (auto-run on startup)
    sqlc/             # Generated code (don't edit directly)
  streaming/          # Stream manager for broadcast streaming + tool execution
  stripe/             # Stripe subscription handling
  task/               # Temporal.io task scheduling
  telegram/           # Telegram bot integration
  tiers/              # Subscription tier definitions (free/plus/pro)
  title_generation/   # Automatic chat title generation via LLM
  tools/              # Tool execution system (exa search, scheduled tasks)
  zcash/              # Zcash payment integration
graph/                # GraphQL schema and resolvers (gqlgen, used by Telegram)
deploy/               # Nitro Enclave configs (enclaver.yaml, envoy.yaml)
docs/                 # Feature documentation (deep research, attestation, etc.)
```

## Key Patterns

**Database changes**: Add SQL to `internal/storage/pg/queries/`, run `make sqlc`, use generated methods in `sqlc/`.

**Migrations**: Add numbered file to `internal/storage/pg/migrations/` with `-- +goose Up/Down` directives. Auto-runs on startup.

**New endpoints**: Add handler in relevant `internal/` package, register in `cmd/server/main.go` (`setupRESTServer` function).

**Tier/quota changes**: Edit `internal/tiers/tiers.go` for limits. Token multipliers are in `config/config.yaml` per model.

**Model routing changes**: Edit `config/config.yaml` to add models, aliases, providers, or fallback rules.

**Adding new tools**: Implement `tools.Tool` interface, register in `cmd/server/main.go` via `toolRegistry.Register()`.

## Config

Two config sources:
1. **Environment variables**: All defined in `internal/config/config.go` with defaults
2. **config.yaml**: Model routing configuration (required, path via `CONFIG_FILE` env var)

Key env vars: `DATABASE_URL`, `FIREBASE_PROJECT_ID`, `FIREBASE_CRED_JSON`, `OPENAI_API_KEY`, `OPENROUTER_MOBILE_API_KEY`

## Gotchas

- **config.yaml is required** - Server fails to start without it. Contains model→provider routing.
- **Two API servers** - REST on `:8080`, GraphQL on `:8081` (only if Telegram enabled).
- **StreamManager handles client disconnects** - Continues streaming to completion even if client disconnects, stores result in Firestore.
- **Fallback routing uses Prometheus** - Requires `FALLBACK_PROMETHEUS_URL` for self-hosted model health-based routing.
- **Auth middleware is global** - Applied in `setupRESTServer`, exceptions only for webhooks (`/stripe/webhook`).
- **OpenRouter has platform-specific keys** - `mobile` vs `desktop` keys selected based on `X-Client-Platform` header.
