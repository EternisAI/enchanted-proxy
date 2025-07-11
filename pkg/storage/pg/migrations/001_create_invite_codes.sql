-- +goose Up
CREATE TABLE IF NOT EXISTS invite_codes (
    id            BIGSERIAL PRIMARY KEY,
    code          TEXT        NOT NULL,
    code_hash     TEXT        NOT NULL UNIQUE,
    bound_email   TEXT,
    created_by    BIGINT      NOT NULL,
    is_used       BOOLEAN     NOT NULL DEFAULT false,
    redeemed_by   TEXT,
    redeemed_at   TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    is_active     BOOLEAN     NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_invite_codes_code_hash ON invite_codes (code_hash);
CREATE INDEX IF NOT EXISTS idx_invite_codes_redeemed_by ON invite_codes (redeemed_by);
CREATE INDEX IF NOT EXISTS idx_invite_codes_deleted_at ON invite_codes (deleted_at);

-- +goose Down
DROP INDEX IF EXISTS idx_invite_codes_deleted_at;
DROP INDEX IF EXISTS idx_invite_codes_redeemed_by;
DROP INDEX IF EXISTS idx_invite_codes_code_hash;
DROP TABLE IF EXISTS invite_codes;