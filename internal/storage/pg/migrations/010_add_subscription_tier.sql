-- +goose Up
-- Migration: Add subscription tier column and rename pro_expires_at to subscription_expires_at
-- Purpose: Store user's subscription tier (free/pro) for new rate limiting system
-- Also rename pro_expires_at to be more generic (applies to any subscription type, not just "pro")
ALTER TABLE entitlements
ADD COLUMN IF NOT EXISTS subscription_tier TEXT NOT NULL DEFAULT 'free';

-- Backfill existing data: map pro_expires_at to tier
UPDATE entitlements
SET subscription_tier = CASE
    WHEN pro_expires_at IS NOT NULL AND pro_expires_at > NOW() THEN 'pro'
    ELSE 'free'
END;

-- Rename pro_expires_at to subscription_expires_at for clarity
ALTER TABLE entitlements
RENAME COLUMN pro_expires_at TO subscription_expires_at;

-- Create index for tier lookups
CREATE INDEX IF NOT EXISTS idx_entitlements_tier
ON entitlements (user_id, subscription_tier);

-- +goose Down
ALTER TABLE entitlements RENAME COLUMN subscription_expires_at TO pro_expires_at;
DROP INDEX IF EXISTS idx_entitlements_tier;
ALTER TABLE entitlements DROP COLUMN IF EXISTS subscription_tier;
