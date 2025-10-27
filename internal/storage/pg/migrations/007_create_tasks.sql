-- +goose Up
CREATE TABLE IF NOT EXISTS tasks (
    task_id       TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    chat_id       TEXT NOT NULL,
    task_name     TEXT NOT NULL,
    task_text     TEXT NOT NULL,
    type          TEXT NOT NULL, -- 'recurring' or 'one_time'
    time          TEXT NOT NULL, -- cron format for both types
    status        TEXT NOT NULL DEFAULT 'pending',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_tasks_user_id ON tasks (user_id);
CREATE INDEX IF NOT EXISTS idx_tasks_chat_id ON tasks (chat_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks (status);
CREATE INDEX IF NOT EXISTS idx_tasks_user_task ON tasks (user_id, task_id);

-- +goose Down
DROP INDEX IF EXISTS idx_tasks_user_task;
DROP INDEX IF EXISTS idx_tasks_status;
DROP INDEX IF EXISTS idx_tasks_chat_id;
DROP INDEX IF EXISTS idx_tasks_user_id;
DROP TABLE IF EXISTS tasks;
