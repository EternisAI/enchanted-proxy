-- name: InsertApplePurchase :execrows
-- Records a purchase seen for the first time, and reports whether it did. A
-- purchase recorded meanwhile by another request is left as it is; the caller
-- then locks and reads it.
INSERT INTO apple_purchases (
    original_transaction_id, product_id, environment, tier, expires_at,
    holder_user_id, last_transaction_id
) VALUES (
    sqlc.arg(original_transaction_id), sqlc.arg(product_id), sqlc.arg(environment), sqlc.arg(tier),
    sqlc.narg(expires_at), sqlc.arg(holder_user_id), sqlc.arg(last_transaction_id)
)
ON CONFLICT (original_transaction_id) DO NOTHING;

-- name: GetApplePurchase :one
SELECT * FROM apple_purchases
WHERE original_transaction_id = $1;

-- name: LockApplePurchase :one
-- Reads a purchase and holds its row until the transaction ends, so attaches
-- of the same purchase are made one at a time.
SELECT * FROM apple_purchases
WHERE original_transaction_id = $1
FOR UPDATE;

-- name: UpdateApplePurchase :exec
UPDATE apple_purchases
SET product_id          = sqlc.arg(product_id),
    environment         = sqlc.arg(environment),
    tier                = sqlc.arg(tier),
    expires_at          = sqlc.narg(expires_at),
    holder_user_id      = sqlc.arg(holder_user_id),
    last_transaction_id = sqlc.arg(last_transaction_id),
    updated_at          = NOW()
WHERE original_transaction_id = sqlc.arg(original_transaction_id);

-- name: BlockApplePurchase :exec
-- Blocks a purchase, with when Apple revoked it if that is the reason.
UPDATE apple_purchases
SET blocked_at = NOW(), block_reason = sqlc.arg(reason),
    revoked_at = COALESCE(sqlc.narg(revoked_at), revoked_at), updated_at = NOW()
WHERE original_transaction_id = sqlc.arg(original_transaction_id)
  AND blocked_at IS NULL;

-- name: RecordApplePurchaseEvent :exec
INSERT INTO apple_purchase_events (original_transaction_id, kind, user_id, previous_user_id, transaction_id, reason)
VALUES (
    sqlc.arg(original_transaction_id), sqlc.arg(kind), sqlc.arg(user_id),
    sqlc.narg(previous_user_id), sqlc.narg(transaction_id), sqlc.narg(reason)
);

-- name: CountApplePurchaseAccountsSince :one
-- The accounts a purchase was attached to since a time: each account it was
-- attached or moved to, and each account it was moved away from.
SELECT COUNT(DISTINCT a.account)::int AS accounts
FROM apple_purchase_events e
CROSS JOIN LATERAL (VALUES (e.user_id), (e.previous_user_id)) AS a(account)
WHERE e.original_transaction_id = sqlc.arg(purchase_id)
  AND e.kind IN ('attach', 'move')
  AND e.created_at >= sqlc.arg(since);

-- name: LockEntitlement :one
-- Reads an account's entitlement and holds its row until the transaction ends.
SELECT user_id, subscription_tier, subscription_expires_at, subscription_provider, apple_original_transaction_id
FROM entitlements
WHERE user_id = $1
FOR UPDATE;

-- name: SetAppleEntitlement :exec
-- Writes an entitlement granted by an Apple purchase. A Stripe customer ID on
-- the row is kept.
INSERT INTO entitlements (user_id, subscription_tier, subscription_expires_at, subscription_provider, apple_original_transaction_id, updated_at)
VALUES (sqlc.arg(user_id), sqlc.arg(subscription_tier), sqlc.narg(subscription_expires_at), 'apple', sqlc.arg(apple_original_transaction_id), NOW())
ON CONFLICT (user_id) DO UPDATE SET
  subscription_tier             = EXCLUDED.subscription_tier,
  subscription_expires_at       = EXCLUDED.subscription_expires_at,
  subscription_provider         = 'apple',
  apple_original_transaction_id = EXCLUDED.apple_original_transaction_id,
  updated_at                    = NOW();

-- name: ResetApplePurchaseEntitlement :execrows
-- Sets one account back to Free, if its entitlement came from the purchase.
UPDATE entitlements
SET subscription_tier = 'free', subscription_expires_at = NULL, updated_at = NOW()
WHERE user_id = sqlc.arg(user_id)
  AND subscription_provider = 'apple'
  AND apple_original_transaction_id = sqlc.arg(apple_original_transaction_id)
  AND subscription_tier <> 'free';

-- name: ResetApplePurchaseEntitlements :many
-- Sets every account whose entitlement came from the purchase back to Free.
UPDATE entitlements
SET subscription_tier = 'free', subscription_expires_at = NULL, updated_at = NOW()
WHERE subscription_provider = 'apple'
  AND apple_original_transaction_id = sqlc.arg(apple_original_transaction_id)
  AND subscription_tier <> 'free'
RETURNING user_id;
