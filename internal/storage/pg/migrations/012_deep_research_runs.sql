-- +goose Up
-- Migration: Track deep research runs for quota enforcement
-- Purpose: Enforce "1 free deep research lifetime" and "10 pro deep research daily" limits
-- Also enforces per-run token caps (8k free, 10k pro) and max active sessions (1)
CREATE TABLE IF NOT EXISTS deep_research_runs (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            TEXT        NOT NULL,
    chat_id            TEXT        NOT NULL,
    run_date           DATE        NOT NULL,  -- For daily quota (Pro)
    model_tokens_used  INTEGER     NOT NULL DEFAULT 0,  -- GLM-4.6 tokens (unweighted)
    plan_tokens_used   INTEGER     NOT NULL DEFAULT 0,  -- model_tokens × 3
    status             TEXT        NOT NULL,  -- 'active', 'completed', 'failed'
    started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at       TIMESTAMPTZ
);

-- Indexes for quota checks
CREATE INDEX IF NOT EXISTS idx_deep_research_runs_user_date
ON deep_research_runs (user_id, run_date);

CREATE INDEX IF NOT EXISTS idx_deep_research_runs_user_lifetime
ON deep_research_runs (user_id, started_at);

CREATE INDEX IF NOT EXISTS idx_deep_research_runs_active
ON deep_research_runs (user_id, status)
WHERE status = 'active';

-- +goose Down
DROP TABLE IF EXISTS deep_research_runs;
