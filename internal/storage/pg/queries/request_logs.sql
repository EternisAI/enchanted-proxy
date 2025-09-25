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

-- name: GetUserRequestCountToday :one
SELECT COALESCE(SUM(request_count), 0)::BIGINT as total_requests
FROM user_request_counts_daily
WHERE user_id = $1
  AND day_bucket = DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC');

-- name: RefreshUserRequestCountsView :exec
REFRESH MATERIALIZED VIEW CONCURRENTLY user_request_counts_daily;

-- name: RefreshUserTokenUsageView :exec
REFRESH MATERIALIZED VIEW CONCURRENTLY user_token_usage_daily; 