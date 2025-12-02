-- name: UpsertEntitlement :exec
INSERT INTO entitlements (user_id, pro_expires_at, subscription_provider, updated_at)
VALUES ($1, $2, $3, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  pro_expires_at = EXCLUDED.pro_expires_at,
  subscription_provider = EXCLUDED.subscription_provider,
  updated_at     = NOW();

-- name: GetEntitlement :one
SELECT user_id, pro_expires_at, subscription_provider, updated_at
FROM entitlements
WHERE user_id = $1;