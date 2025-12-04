-- +goose Up
-- Migration: Add plan token tracking with model-based cost multipliers
-- Purpose: Support weighted token accounting where different models have different costs
-- Example: 1,000 GPT-5 Pro tokens = 50,000 plan tokens (50× multiplier)
-- Note: We use existing total_tokens column (no new model_tokens column needed)
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS plan_tokens INTEGER,           -- total_tokens × multiplier (user-visible quota)
ADD COLUMN IF NOT EXISTS token_multiplier NUMERIC(8,2); -- Audit trail for multiplier (e.g., 1.00, 50.00)

-- Backfill existing data: assume 1× multiplier for all historical records
UPDATE request_logs
SET plan_tokens = total_tokens,
    token_multiplier = 1.0
WHERE total_tokens IS NOT NULL
  AND plan_tokens IS NULL;

-- Create index for plan token queries (monthly and daily aggregations)
CREATE INDEX IF NOT EXISTS idx_request_logs_plan_tokens
ON request_logs (user_id, created_at, plan_tokens)
WHERE plan_tokens IS NOT NULL;

-- Update materialized view to aggregate plan tokens
DROP MATERIALIZED VIEW IF EXISTS user_token_usage_daily CASCADE;

-- Extend retention to 30 days (needed for monthly quota checks)
CREATE MATERIALIZED VIEW user_token_usage_daily AS
SELECT
    user_id,
    DATE_TRUNC('day', created_at) as day_bucket,
    COUNT(*) as request_count,
    COALESCE(SUM(prompt_tokens), 0) as total_prompt_tokens,
    COALESCE(SUM(completion_tokens), 0) as total_completion_tokens,
    COALESCE(SUM(total_tokens), 0) as total_tokens_used,
    COALESCE(SUM(plan_tokens), 0) as total_plan_tokens         -- NEW
FROM request_logs
WHERE created_at >= NOW() - INTERVAL '30 days'
GROUP BY user_id, DATE_TRUNC('day', created_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_token_usage_daily_unique
ON user_token_usage_daily (user_id, day_bucket);

CREATE INDEX IF NOT EXISTS idx_user_token_usage_daily_plan_tokens
ON user_token_usage_daily (user_id, total_plan_tokens);

-- +goose Down
DROP INDEX IF EXISTS idx_user_token_usage_daily_plan_tokens;
DROP MATERIALIZED VIEW IF EXISTS user_token_usage_daily;
DROP INDEX IF EXISTS idx_request_logs_plan_tokens;
ALTER TABLE request_logs
DROP COLUMN IF EXISTS token_multiplier,
DROP COLUMN IF EXISTS plan_tokens;
