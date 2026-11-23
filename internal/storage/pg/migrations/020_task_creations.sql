-- +goose Up
-- One row per task created, kept when the task is deleted, so the number of
-- tasks an account creates per day can be limited.
CREATE TABLE IF NOT EXISTS task_creations (
    id         BIGSERIAL PRIMARY KEY,
    user_id    TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_task_creations_user_created ON task_creations (user_id, created_at);

-- +goose Down
DROP TABLE IF EXISTS task_creations;
