-- +goose Up
CREATE TABLE IF NOT EXISTS request_logs (
    id          BIGSERIAL PRIMARY KEY,
    user_id     TEXT        NOT NULL,
    endpoint    TEXT        NOT NULL,
    model       TEXT,
    provider    TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_request_logs_user_id ON request_logs (user_id);
CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs (created_at);
CREATE INDEX IF NOT EXISTS idx_request_logs_user_created ON request_logs (user_id, created_at);

-- Materialized view for fast rate limiting queries
CREATE MATERIALIZED VIEW IF NOT EXISTS user_request_counts_daily AS
SELECT 
    user_id,
    DATE_TRUNC('day', created_at) as day_bucket,
    COUNT(*) as request_count
FROM request_logs 
WHERE created_at >= NOW() - INTERVAL '7 days'
GROUP BY user_id, DATE_TRUNC('day', created_at);

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_request_counts_daily_unique 
ON user_request_counts_daily (user_id, day_bucket);

-- +goose Down
DROP MATERIALIZED VIEW IF EXISTS user_request_counts_daily;
DROP INDEX IF EXISTS idx_request_logs_user_created;
DROP INDEX IF EXISTS idx_request_logs_created_at;
DROP INDEX IF EXISTS idx_request_logs_user_id;
DROP TABLE IF EXISTS request_logs;
