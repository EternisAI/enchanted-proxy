-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS problem_reports (
    id                       TEXT PRIMARY KEY,
    user_id                  TEXT NOT NULL,
    problem_description      TEXT NOT NULL,
    
    -- Device info (flattened from iOS DeviceInfo struct)
    device_model             TEXT,
    device_name              TEXT,
    system_name              TEXT,
    system_version           TEXT,
    app_version              TEXT,
    build_number             TEXT,
    locale                   TEXT,
    timezone                 TEXT,
    
    -- Storage info (from iOS StorageInfo struct)
    total_capacity_bytes     BIGINT,
    available_capacity_bytes BIGINT,
    used_capacity_bytes      BIGINT,
    
    -- Subscription info
    subscription_tier        TEXT,
    
    -- Contact info (optional, for replies)
    contact_email            TEXT,
    
    -- Duplicate detection and Linear integration
    parent_id                TEXT REFERENCES problem_reports(id),
    ticket_id                TEXT,
    embedding                vector(1536),
    
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_problem_reports_user_id ON problem_reports (user_id);
CREATE INDEX IF NOT EXISTS idx_problem_reports_parent_id ON problem_reports (parent_id);
CREATE INDEX IF NOT EXISTS idx_problem_reports_ticket_id ON problem_reports (ticket_id);
CREATE INDEX IF NOT EXISTS idx_problem_reports_created_at ON problem_reports (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_problem_reports_embedding ON problem_reports USING ivfflat (embedding vector_cosine_ops) WITH (lists = 100);

-- +goose Down
DROP INDEX IF EXISTS idx_problem_reports_embedding;
DROP INDEX IF EXISTS idx_problem_reports_created_at;
DROP INDEX IF EXISTS idx_problem_reports_ticket_id;
DROP INDEX IF EXISTS idx_problem_reports_parent_id;
DROP INDEX IF EXISTS idx_problem_reports_user_id;
DROP TABLE IF EXISTS problem_reports;
