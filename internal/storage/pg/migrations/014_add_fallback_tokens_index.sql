-- +goose Up
-- Index for GetUserFallbackPlanTokensToday query
-- Optimizes: WHERE user_id = $1 AND model = $2 AND created_at >= today AND plan_tokens IS NOT NULL
CREATE INDEX IF NOT EXISTS idx_request_logs_fallback_tokens
ON request_logs (user_id, model, created_at, plan_tokens)
WHERE plan_tokens IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_request_logs_fallback_tokens;
