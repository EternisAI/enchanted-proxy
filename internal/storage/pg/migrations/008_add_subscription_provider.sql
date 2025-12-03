-- +goose Up
ALTER TABLE entitlements
ADD COLUMN IF NOT EXISTS subscription_provider TEXT NULL;

COMMENT ON COLUMN entitlements.subscription_provider IS 'Subscription source: apple, stripe, or NULL for free tier';

-- +goose Down
ALTER TABLE entitlements
DROP COLUMN IF EXISTS subscription_provider;
