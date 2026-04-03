-- name: CreateRequestLog :exec
INSERT INTO request_logs (user_id, endpoint, model, provider, prompt_tokens, completion_tokens, total_tokens) 
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CreateRequestLogWithPlanTokens :exec
INSERT INTO request_logs (
    user_id, endpoint, model, provider,
    prompt_tokens, completion_tokens, total_tokens,
    plan_tokens, token_multiplier
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetUserPlanTokensToday :one
-- Queries request_logs directly for real-time data (not materialized view).
-- Performance: The idx_request_logs_plan_tokens index on (user_id, created_at, plan_tokens) keeps this fast.
SELECT COALESCE(SUM(plan_tokens), 0)::BIGINT as plan_tokens
FROM request_logs
WHERE user_id = $1
  AND created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC')
  AND plan_tokens IS NOT NULL;

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
-- Returns plan tokens used today on the fallback model.
-- Used for tracking fallback quota when normal quota is exceeded.
SELECT COALESCE(SUM(plan_tokens), 0)::BIGINT as plan_tokens
FROM request_logs
WHERE user_id = $1
  AND created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC')
  AND plan_tokens IS NOT NULL
  AND model = $2;