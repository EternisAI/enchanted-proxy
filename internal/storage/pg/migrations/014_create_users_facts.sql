-- +goose Up
CREATE TABLE IF NOT EXISTS users_facts (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    fact_body     TEXT NOT NULL,
    fact_type     TEXT NOT NULL, -- 'work_context', 'personal_context', 'top_of_mind'
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_users_facts_user_id ON users_facts (user_id);
CREATE INDEX IF NOT EXISTS idx_users_facts_fact_type ON users_facts (fact_type);
CREATE INDEX IF NOT EXISTS idx_users_facts_user_type ON users_facts (user_id, fact_type);

-- +goose Down
DROP INDEX IF EXISTS idx_users_facts_user_type;
DROP INDEX IF EXISTS idx_users_facts_fact_type;
DROP INDEX IF EXISTS idx_users_facts_user_id;
DROP TABLE IF EXISTS users_facts;
