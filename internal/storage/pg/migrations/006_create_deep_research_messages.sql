-- +goose Up
CREATE TABLE IF NOT EXISTS deep_research_messages (
    id              TEXT        PRIMARY KEY,
    user_id         TEXT        NOT NULL,
    chat_id         TEXT        NOT NULL,
    session_id      TEXT        NOT NULL, -- user_id__chat_id combined (double underscore separator)
    message         TEXT        NOT NULL,
    message_type    TEXT        NOT NULL, -- status, error, research_complete, etc.
    sent            BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_deep_research_messages_session_id ON deep_research_messages (session_id);
CREATE INDEX IF NOT EXISTS idx_deep_research_messages_user_chat ON deep_research_messages (user_id, chat_id);
CREATE INDEX IF NOT EXISTS idx_deep_research_messages_sent ON deep_research_messages (session_id, sent);
CREATE INDEX IF NOT EXISTS idx_deep_research_messages_created_at ON deep_research_messages (session_id, created_at);

-- +goose Down
DROP INDEX IF EXISTS idx_deep_research_messages_created_at;
DROP INDEX IF EXISTS idx_deep_research_messages_sent;
DROP INDEX IF EXISTS idx_deep_research_messages_user_chat;
DROP INDEX IF EXISTS idx_deep_research_messages_session_id;
DROP TABLE IF EXISTS deep_research_messages;
