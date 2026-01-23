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

-- name: UpsertEntitlementWithExtension :exec
-- Grants or extends an entitlement. For same-tier renewals where the current
-- subscription is still active (expires after invoice creation), extends from
-- the current expiration. Otherwise starts from the provided base time.
INSERT INTO entitlements (user_id, subscription_tier, subscription_expires_at, subscription_provider, stripe_customer_id, updated_at)
VALUES (
  sqlc.arg(user_id),
  sqlc.arg(subscription_tier),
  sqlc.arg(base_time)::timestamptz + (interval '1 day' * sqlc.arg(duration_days)::int),
  sqlc.arg(subscription_provider),
  sqlc.arg(stripe_customer_id),
  NOW()
)
ON CONFLICT (user_id) DO UPDATE SET
  subscription_tier = sqlc.arg(subscription_tier),
  subscription_expires_at =
    CASE
      -- Same tier and current subscription expires after invoice creation: extend
      WHEN entitlements.subscription_tier = sqlc.arg(subscription_tier)
           AND entitlements.subscription_expires_at IS NOT NULL
           AND entitlements.subscription_expires_at > sqlc.arg(base_time)::timestamptz
      THEN entitlements.subscription_expires_at + (interval '1 day' * sqlc.arg(duration_days)::int)
      -- Different tier or expired: start fresh from base time
      ELSE sqlc.arg(base_time)::timestamptz + (interval '1 day' * sqlc.arg(duration_days)::int)
    END,
  subscription_provider = sqlc.arg(subscription_provider),
  stripe_customer_id = COALESCE(sqlc.arg(stripe_customer_id), entitlements.stripe_customer_id),
  updated_at = NOW();