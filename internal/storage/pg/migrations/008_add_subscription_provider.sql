-- +goose Up
ALTER TABLE entitlements
ADD COLUMN IF NOT EXISTS subscription_provider TEXT NOT NULL DEFAULT 'apple';

COMMENT ON COLUMN entitlements.subscription_provider IS 'Subscription source: apple, stripe';

-- +goose Down
ALTER TABLE entitlements
DROP COLUMN IF EXISTS subscription_provider;
