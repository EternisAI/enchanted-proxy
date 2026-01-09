-- +goose Up
-- Add fallback rate limit tracking
ALTER TABLE request_logs
ADD COLUMN IF NOT EXISTS is_fallback_request BOOLEAN DEFAULT FALSE NOT NULL;

-- +goose Down
ALTER TABLE request_logs
DROP COLUMN IF EXISTS is_fallback_request;
