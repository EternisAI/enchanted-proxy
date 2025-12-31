-- +goose Up
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

    -- Linear integration
    ticket_id                TEXT,

    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_problem_reports_user_id ON problem_reports (user_id);
CREATE INDEX IF NOT EXISTS idx_problem_reports_ticket_id ON problem_reports (ticket_id);
CREATE INDEX IF NOT EXISTS idx_problem_reports_created_at ON problem_reports (created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_problem_reports_created_at;
DROP INDEX IF EXISTS idx_problem_reports_ticket_id;
DROP INDEX IF EXISTS idx_problem_reports_user_id;
DROP TABLE IF EXISTS problem_reports;
