-- name: UpsertEntitlement :exec
INSERT INTO entitlements (user_id, pro_expires_at, subscription_provider, stripe_customer_id, updated_at)
VALUES ($1, $2, $3, $4, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  pro_expires_at = EXCLUDED.pro_expires_at,
  subscription_provider = EXCLUDED.subscription_provider,
  stripe_customer_id = COALESCE(EXCLUDED.stripe_customer_id, entitlements.stripe_customer_id),
  updated_at     = NOW();

-- name: GetEntitlement :one
SELECT user_id, pro_expires_at, subscription_provider, stripe_customer_id, updated_at
FROM entitlements
WHERE user_id = $1;

-- name: GetStripeCustomerID :one
SELECT stripe_customer_id
FROM entitlements
WHERE user_id = $1
  AND stripe_customer_id IS NOT NULL;