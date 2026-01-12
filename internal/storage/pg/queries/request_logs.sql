-- name: CreateRequestLog :exec
INSERT INTO request_logs (user_id, endpoint, model, provider, prompt_tokens, completion_tokens, total_tokens) 
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetUserRequestCountInTimeWindow :one
SELECT COALESCE(SUM(request_count), 0)::BIGINT as total_requests
FROM user_request_counts_daily
WHERE user_id = $1
  AND day_bucket >= $2;

-- name: GetUserRequestCountInLastDay :one
SELECT COALESCE(SUM(request_count), 0)::BIGINT as total_requests
FROM user_request_counts_daily
WHERE user_id = $1
  AND day_bucket = DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC');

-- name: GetUserTokenUsageInTimeWindow :one
SELECT COALESCE(SUM(total_tokens_used), 0)::BIGINT as total_tokens
FROM user_token_usage_daily
WHERE user_id = $1
  AND day_bucket >= $2;

-- name: GetUserTokenUsageInLastDay :one
SELECT COALESCE(SUM(total_tokens_used), 0)::BIGINT as total_tokens
FROM user_token_usage_daily
WHERE user_id = $1
  AND day_bucket = DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC');

-- name: GetUserLifetimeTokenUsage :one
SELECT COALESCE(SUM(total_tokens), 0)::BIGINT as total_tokens
FROM request_logs
WHERE user_id = $1 AND total_tokens IS NOT NULL;

-- name: GetUserTokenUsageToday :one
SELECT COALESCE(SUM(total_tokens_used), 0)::BIGINT as total_tokens
FROM user_token_usage_daily
WHERE user_id = $1
  AND day_bucket = DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC');

-- name: RefreshUserRequestCountsView :exec
REFRESH MATERIALIZED VIEW CONCURRENTLY user_request_counts_daily;

-- name: RefreshUserTokenUsageView :exec
REFRESH MATERIALIZED VIEW CONCURRENTLY user_token_usage_daily;

-- name: CreateRequestLogWithPlanTokens :exec
INSERT INTO request_logs (
    user_id, endpoint, model, provider,
    prompt_tokens, completion_tokens, total_tokens,
    plan_tokens, token_multiplier
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetUserPlanTokensToday :one
SELECT COALESCE(SUM(total_plan_tokens), 0)::BIGINT as plan_tokens
FROM user_token_usage_daily
WHERE user_id = $1
  AND day_bucket = DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC');

-- name: GetUserPlanTokensThisWeek :one
-- Note: Queries request_logs directly (not materialized view) because weekly buckets aren't pre-aggregated.
-- Performance: The idx_request_logs_plan_tokens index on (user_id, created_at, plan_tokens) keeps this fast (<100ms).
-- Week starts Monday at 00:00 UTC per PostgreSQL DATE_TRUNC('week') behavior.
SELECT COALESCE(SUM(plan_tokens), 0)::BIGINT as plan_tokens
FROM request_logs
WHERE user_id = $1
  AND created_at >= DATE_TRUNC('week', NOW() AT TIME ZONE 'UTC')
  AND plan_tokens IS NOT NULL;

-- name: GetUserPlanTokensThisMonth :one
-- Note: Queries request_logs directly (not materialized view) because monthly buckets aren't pre-aggregated.
-- Performance: The idx_request_logs_plan_tokens index on (user_id, created_at, plan_tokens) keeps this fast (<100ms).
-- Month starts on 1st at 00:00 UTC per PostgreSQL DATE_TRUNC('month') behavior.
SELECT COALESCE(SUM(plan_tokens), 0)::BIGINT as plan_tokens
FROM request_logs
WHERE user_id = $1
  AND created_at >= DATE_TRUNC('month', NOW() AT TIME ZONE 'UTC')
  AND plan_tokens IS NOT NULL;

-- name: GetUserFallbackPlanTokensToday :one
-- Returns plan tokens used today on fallback models (qwen).
-- Used for tracking fallback quota when normal quota is exceeded.
SELECT COALESCE(SUM(plan_tokens), 0)::BIGINT as plan_tokens
FROM request_logs
WHERE user_id = $1
  AND created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC')
  AND plan_tokens IS NOT NULL
  AND model ILIKE '%qwen%';