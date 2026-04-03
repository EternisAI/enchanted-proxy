-- +goose Up
-- Drop unused materialized views. All queries now use request_logs directly
-- with the idx_request_logs_plan_tokens index.
DROP MATERIALIZED VIEW IF EXISTS user_request_counts_daily;
DROP MATERIALIZED VIEW IF EXISTS user_token_usage_daily;

-- +goose Down
-- Recreate user_request_counts_daily
CREATE MATERIALIZED VIEW IF NOT EXISTS user_request_counts_daily AS
SELECT
    user_id,
    DATE_TRUNC('day', created_at AT TIME ZONE 'UTC') as day_bucket,
    COUNT(*) as request_count
FROM request_logs
GROUP BY user_id, DATE_TRUNC('day', created_at AT TIME ZONE 'UTC');

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_request_counts_daily_unique
ON user_request_counts_daily (user_id, day_bucket);

-- Recreate user_token_usage_daily
CREATE MATERIALIZED VIEW IF NOT EXISTS user_token_usage_daily AS
SELECT
    user_id,
    DATE_TRUNC('day', created_at AT TIME ZONE 'UTC') as day_bucket,
    COUNT(*) as request_count,
    COALESCE(SUM(total_tokens), 0) as total_tokens_used,
    COALESCE(SUM(prompt_tokens), 0) as total_prompt_tokens,
    COALESCE(SUM(completion_tokens), 0) as total_completion_tokens,
    COALESCE(SUM(plan_tokens), 0) as total_plan_tokens
FROM request_logs
GROUP BY user_id, DATE_TRUNC('day', created_at AT TIME ZONE 'UTC');

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_token_usage_daily_unique
ON user_token_usage_daily (user_id, day_bucket);

CREATE INDEX IF NOT EXISTS idx_user_token_usage_daily_plan_tokens
ON user_token_usage_daily (user_id, total_plan_tokens);
