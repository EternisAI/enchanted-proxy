-- +goose Up
CREATE TABLE IF NOT EXISTS entitlements (
    user_id        TEXT        PRIMARY KEY,
    pro_expires_at TIMESTAMPTZ NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS entitlements;