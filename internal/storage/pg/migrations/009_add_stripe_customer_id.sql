-- +goose Up
ALTER TABLE entitlements
ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT NULL;

COMMENT ON COLUMN entitlements.stripe_customer_id IS 'Stripe Customer ID for billing portal access (cus_xxx)';

-- +goose Down
ALTER TABLE entitlements
DROP COLUMN IF EXISTS stripe_customer_id;
