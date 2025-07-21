-- +goose Up
CREATE TABLE IF NOT EXISTS telegram_chats (
    id          BIGSERIAL PRIMARY KEY,
    chat_id     BIGINT      NOT NULL UNIQUE,
    chat_uuid   TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_telegram_chats_chat_uuid ON telegram_chats (chat_uuid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_telegram_chats_chat_id ON telegram_chats (chat_id);

-- +goose Down
DROP INDEX IF EXISTS idx_telegram_chats_chat_uuid;
DROP INDEX IF EXISTS idx_telegram_chats_chat_id;
DROP TABLE IF EXISTS telegram_chats; 