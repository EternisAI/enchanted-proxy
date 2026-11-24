-- +goose Up
-- Apple purchases, one row per original transaction ID. A purchase grants its
-- plan to one account at a time, its holder. A blocked purchase grants nothing.
CREATE TABLE IF NOT EXISTS apple_purchases (
    original_transaction_id TEXT PRIMARY KEY,
    product_id              TEXT NOT NULL,
    environment             TEXT NOT NULL,
    tier                    TEXT NOT NULL,
    -- NULL for a lifetime purchase.
    expires_at              TIMESTAMPTZ,
    revoked_at              TIMESTAMPTZ,
    holder_user_id          TEXT NOT NULL,
    last_transaction_id     TEXT NOT NULL,
    blocked_at              TIMESTAMPTZ,
    block_reason            TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_apple_purchases_holder ON apple_purchases (holder_user_id);

-- Append-only record of what happened to each purchase: attached to its first
-- account (attach), moved to another account (move), blocked or unblocked.
CREATE TABLE IF NOT EXISTS apple_purchase_events (
    id                      BIGSERIAL PRIMARY KEY,
    original_transaction_id TEXT NOT NULL,
    kind                    TEXT NOT NULL,
    user_id                 TEXT NOT NULL,
    previous_user_id        TEXT,
    transaction_id          TEXT,
    reason                  TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_apple_purchase_events_purchase ON apple_purchase_events (original_transaction_id, created_at);

-- Quota groups: accounts whose usage counts together, because a purchase has
-- been attached to each of them. An account with no row is a group of one.
CREATE TABLE IF NOT EXISTS quota_group_members (
    user_id   TEXT PRIMARY KEY,
    group_id  TEXT NOT NULL,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_quota_group_members_group ON quota_group_members (group_id);

-- The purchase an Apple entitlement came from. NULL on rows written before
-- purchases were recorded, and on rows from other providers.
ALTER TABLE entitlements ADD COLUMN IF NOT EXISTS apple_original_transaction_id TEXT;

CREATE INDEX IF NOT EXISTS idx_entitlements_apple_purchase ON entitlements (apple_original_transaction_id)
    WHERE apple_original_transaction_id IS NOT NULL;

-- One row per run of the entitlement review: when, in which mode, and what it
-- did or would do.
CREATE TABLE IF NOT EXISTS entitlement_review_runs (
    id          BIGSERIAL PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    mode        TEXT NOT NULL,
    report      JSONB
);

-- A copy of the entitlements table taken by each enforced review run before it
-- changes anything.
CREATE TABLE IF NOT EXISTS entitlements_review_backup (
    run_id                  BIGINT NOT NULL,
    user_id                 TEXT NOT NULL,
    subscription_tier       TEXT NOT NULL,
    subscription_expires_at TIMESTAMPTZ,
    subscription_provider   TEXT NOT NULL,
    stripe_customer_id      TEXT,
    updated_at              TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (run_id, user_id)
);

-- +goose Down
DROP TABLE IF EXISTS entitlements_review_backup;
DROP TABLE IF EXISTS entitlement_review_runs;
DROP INDEX IF EXISTS idx_entitlements_apple_purchase;
ALTER TABLE entitlements DROP COLUMN IF EXISTS apple_original_transaction_id;
DROP TABLE IF EXISTS quota_group_members;
DROP TABLE IF EXISTS apple_purchase_events;
DROP TABLE IF EXISTS apple_purchases;
