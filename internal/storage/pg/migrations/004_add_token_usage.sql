-- +goose Up
-- Add token usage columns to request_logs table
ALTER TABLE request_logs 
ADD COLUMN IF NOT EXISTS prompt_tokens     INTEGER,
ADD COLUMN IF NOT EXISTS completion_tokens INTEGER,
ADD COLUMN IF NOT EXISTS total_tokens      INTEGER;

-- Create index for efficient token-based queries
CREATE INDEX IF NOT EXISTS idx_request_logs_tokens ON request_logs (user_id, created_at, total_tokens) WHERE total_tokens IS NOT NULL;

-- Create materialized view for fast token-based rate limiting queries
CREATE MATERIALIZED VIEW IF NOT EXISTS user_token_usage_daily AS
SELECT 
    user_id,
    DATE_TRUNC('day', created_at) as day_bucket,
    COUNT(*) as request_count,
    COALESCE(SUM(prompt_tokens), 0) as total_prompt_tokens,
    COALESCE(SUM(completion_tokens), 0) as total_completion_tokens,
    COALESCE(SUM(total_tokens), 0) as total_tokens_used
FROM request_logs 
WHERE created_at >= NOW() - INTERVAL '7 days'
GROUP BY user_id, DATE_TRUNC('day', created_at);

-- Create unique index for efficient refreshes and queries
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_token_usage_daily_unique 
ON user_token_usage_daily (user_id, day_bucket);

-- Create additional index for fast token lookups
CREATE INDEX IF NOT EXISTS idx_user_token_usage_daily_tokens 
ON user_token_usage_daily (user_id, total_tokens_used);

-- +goose Down
-- Remove indexes and materialized view
DROP INDEX IF EXISTS idx_user_token_usage_daily_tokens;
DROP INDEX IF EXISTS idx_user_token_usage_daily_unique;
DROP MATERIALIZED VIEW IF EXISTS user_token_usage_daily;
DROP INDEX IF EXISTS idx_request_logs_tokens;

-- Remove token columns
ALTER TABLE request_logs 
DROP COLUMN IF EXISTS total_tokens,
DROP COLUMN IF EXISTS completion_tokens,
DROP COLUMN IF EXISTS prompt_tokens;