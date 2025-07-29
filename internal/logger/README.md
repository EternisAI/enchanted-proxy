# Logging in Enchanted Proxy

This document outlines a set of standards for logging in Enchanted Proxy centered around usability and consistency. All logging should follow these standards.

_This document is a work in progress and will be updated as we go._

## Log Levels

Both production (TEE) and development logs is set to `debug` level for now. But in general, assume that the log level is `info` in production unless otherwise specified. This ensures that we don't flood unnecessary logs in production if we end up using a log aggregation service.

The following log levels can be used when appropriate:

- **Debug**: Detailed information for debugging, only for development.
- **Info**: General information about system functions.
- **Warn**: Warning messages for unusual scenarios.
- **Error**: Error conditions that need attention.

## Standard Attributes

### Service-Level Attributes (Always Present)

- `component`: Component/package name (e.g., "oauth", "auth", "proxy").

### Request-Level Attributes (When Available)

- `request_id`: Unique identifier for the request.
- `user_id`: User identifier from authentication.
- `operation`: High-level operation being performed.

### Operation-Specific Attributes

- `duration`: Time taken for operations.
- `error`: Error message (for error logs).
- `http_method`: HTTP method for web requests.
- `http_status`: HTTP status code for responses.
- `target_url`: Target URL for proxy requests.
- `token_type`: Type of token being processed (Firebase/JWT).

## Logging Patterns

### HTTP Requests

```go
// internal/oauth/handlers.go
log := h.logger.WithContext(c.Request.Context()).WithComponent("oauth_handler")
log.Info("oauth token exchange requested",
    slog.String("platform", req.Platform),
    slog.String("grant_type", req.GrantType))
```

### Operations

```go
// Not used anywhere any more. Keeping it here for reference.
logger.LogOperation(ctx, "token_exchange", func() error {
    // actual operation implementation
    return nil
})
```

### Errors

```go
// cmd/server/main.go
log.Error("invalid url format", slog.String("base_url", baseURL), slog.String("error", err.Error()))
```
