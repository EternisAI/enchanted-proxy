-- name: UpsertEntitlement :exec
INSERT INTO entitlements (user_id, pro_expires_at, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (user_id) DO UPDATE SET
  pro_expires_at = EXCLUDED.pro_expires_at,
  updated_at     = NOW();

-- name: GetEntitlement :one
SELECT user_id, pro_expires_at, updated_at
FROM entitlements
WHERE user_id = $1;


