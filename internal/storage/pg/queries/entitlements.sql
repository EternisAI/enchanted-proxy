-- name: UpsertEntitlement :exec
INSERT INTO entitlements (user_id, subscription_expires_at, subscription_provider, stripe_customer_id, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  subscription_expires_at = EXCLUDED.subscription_expires_at,
  subscription_provider = EXCLUDED.subscription_provider,
  stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, entitlements.stripe_customer_id),
  updated_at     = NOW();

-- name: UpsertEntitlementWithTier :exec
INSERT INTO entitlements (user_id, subscription_tier, subscription_expires_at, subscription_provider, stripe_customer_id, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  subscription_tier = EXCLUDED.subscription_tier,
  subscription_expires_at = EXCLUDED.subscription_expires_at,
  subscription_provider = EXCLUDED.subscription_provider,
  stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, entitlements.stripe_customer_id),
  updated_at = NOW();

-- name: GetUserTier :one
SELECT subscription_tier, subscription_expires_at
FROM entitlements
WHERE user_id = $1;

-- name: GetEntitlement :one
SELECT user_id, subscription_expires_at, subscription_provider, stripe_customer_id, subscription_tier, updated_at
FROM entitlements
WHERE user_id = $1;

-- name: GetStripeCustomerID :one
SELECT stripe_customer_id
FROM entitlements
WHERE user_id = $1
  AND stripe_customer_id IS NOT NULL;