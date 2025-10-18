-- +goose Up
CREATE TABLE IF NOT EXISTS tasks (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    TEXT        NOT NULL,
    chat_id    TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    content    TEXT        NOT NULL,
    cron       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tasks_user_id ON tasks (user_id);
CREATE INDEX IF NOT EXISTS idx_tasks_chat_id ON tasks (chat_id);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks (created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_tasks_created_at;
DROP INDEX IF EXISTS idx_tasks_chat_id;
DROP INDEX IF EXISTS idx_tasks_user_id;
DROP TABLE IF EXISTS tasks;
