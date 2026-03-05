# CLAUDE.md - Enchanted Proxy

> ⚠️ **PUBLIC REPOSITORY** - Production backend, intentionally public for TEE verification. Never commit secrets.

Secure reverse proxy for Silo AI. Runs in AWS Nitro Enclaves (TEE). Routes requests to AI providers, handles auth, rate limiting, subscriptions, and E2EE message storage.

## Commands

```bash
make run          # Dev server (localhost:8080)
make build        # Build all
make test         # Tests with race detector
make lint         # Format and lint
make sqlc         # Regenerate SQL after editing queries/
```

## Key Entry Points

| Area | Start Here |
|------|------------|
| Server setup | `cmd/server/main.go` |
| Auth middleware | `internal/auth/middleware.go` |
| Chat completions | `internal/proxy/handlers.go` |
| Responses API adapter | `internal/responses/adapter.go` |
| Model routing | `internal/routing/model_router.go` |
| Model/provider config | `config/config.yaml` |
| Model fallback | `internal/fallback/service.go` |
| Tier definitions | `internal/tiers/tiers.go` |
| Quota tracking | `internal/request_tracking/service.go` |
| Stream management | `internal/streaming/manager.go` |
| Background polling | `internal/background/polling_manager.go` |
| E2EE encryption | `internal/messaging/encryption.go` |
| Key sharing (WS) | `internal/keyshare/handlers.go` |
| Deep research | `internal/deepr/handlers.go` |
| Title generation | `internal/title_generation/service.go` |
| Web search | `internal/search/handlers.go` |
| Tool execution | `internal/tools/registry.go` |
| MCP protocol | `internal/mcp/handlers.go` |
| Scheduled tasks | `internal/task/handlers.go` |
| Notifications (FCM) | `internal/notifications/service.go` |
| Stripe payments | `internal/stripe/handler.go` |
| Zcash payments | `internal/zcash/handler.go` |
| IAP validation | `internal/iap/handler.go` |
| Composio integration | `internal/composio/handlers.go` |
| OAuth token exchange | `internal/oauth/handlers.go` |
| Invite codes | `internal/invitecode/handlers.go` |
| Problem reports | `internal/problem_reports/handler.go` |
| Telegram bot | `internal/telegram/service.go` |

## Tier Limits (Gotcha: Values Must Match Client Apps)

| Tier | Plan Tokens | Reset | Deep Research | Fallback |
|------|-------------|-------|---------------|----------|
| Free | 20k monthly | 1st of month | 1 lifetime | None |
| Plus | 40k daily | Daily 00:00 UTC | Unlimited | 40k/day |
| Pro | 500k daily | Daily 00:00 UTC | 10/day | 500k/day |

**Model multipliers**: 0.04× (Qwen3), 0.5× (Dolphin), 1× (DeepSeek, Llama), 3× (GLM-4.6), 4× (GPT-4.1), 6× (GPT-5.2), 70× (GPT-5.2 Pro)

See `TIER_LIMITS.md` for complete reference.

## E2EE Constants (Critical: Must Match iOS/Web/Proxy)

```go
ACCOUNT_KEY_VERSION       = 1
KEK_LENGTH                = 32    // bytes
AES_GCM_IV_LENGTH         = 12    // bytes
AES_GCM_TAG_LENGTH        = 16    // bytes
KEK_DERIVATION_INFO       = "silo-kek-v1"
MESSAGE_ENCRYPTION_INFO   = "message-encryption"
EPHEMERAL_PUBLIC_KEY_SIZE = 65    // uncompressed P-256
```

Wire format: `base64(ephemeralPubKey[65] || nonce[12] || ciphertext || tag[16])`

Cross-platform tests: `test-vectors/encryption-compatibility.json`

## Model Routing via config.yaml

All model and provider definitions live in `config/config.yaml` (loaded via `CONFIG_FILE` env var). This is the single source of truth for which models are available and how requests get routed.

**Flow**: Client sends `model` field in request → `ProxyHandler` extracts it → `ModelRouter.RouteModel()` resolves it against config → request is forwarded to the matched provider with the correct base URL, API key, and model name.

**Config structure**:
- `model_router.providers` — provider name, base URL, API key env var
- `model_router.models` — canonical model name, aliases, token multiplier, provider list
- `title_generation` — system prompts for conversation title generation

**Resolution order**: exact match → alias match → prefix match → wildcard fallback (OpenRouter).

Dev config: `config/config.dev.yaml` (redirects to local Ollama). Run with `make run-dev`.

## Unauth Routes (No Auth Middleware)

- `/stripe/webhook` - Stripe (signature verified)
- `/wa` - WhatsApp
- `/internal/zcash/callback` - Zcash payment callbacks (static API key verified)

## Development Patterns

**Add a model**: Edit `config/config.yaml` → redeploy (no code change)

**Add a provider**: Add to `config/config.yaml` + add API key env var to deployment config

**Add a tier**: Edit `internal/tiers/tiers.go` + add Stripe product

**Add an endpoint**: Create handler in `internal/<feature>/handlers.go` → register in `cmd/server/main.go`

**Database changes**: Edit `internal/storage/pg/queries/*.sql` → `make sqlc` → use generated methods

**Migrations**: Add numbered file to `internal/storage/pg/migrations/` with `-- +goose Up/Down`

## Testing

```bash
go test ./internal/routing -v      # Model routing
go test ./internal/tiers -v        # Tier limits
make test                          # All tests
```

## Deployment

**TEE config**: `deploy/enclaver.yaml` - egress allowlist for providers. For the full enclave architecture (networking, egress filtering, DNS, process supervision), see [`docs/tee-architecture.md`](docs/tee-architecture.md). Check there first if you hit unexplained networking or connection issues.

**Pre-deploy checklist**:
- All API keys set (OPENAI_API_KEY, STRIPE_API_KEY, etc.)
- Firebase credentials loaded
- DATABASE_URL valid
- Stripe webhook secret configured
- TEE allowlist updated if adding provider

## Gotchas

- Auth middleware applied globally in `main.go` (exceptions listed above)
- E2EE encryption coordinated across iOS/Web/Proxy - constants MUST match
- Structured logging: use `logger.WithComponent("mycomponent")`
- All secrets via environment (never hardcoded - public repo)
- Migrations auto-run on startup
