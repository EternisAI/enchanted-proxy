-- name: GetAppleTransaction :one
SELECT original_transaction_id, user_id, product_id, tier, redeemed_at
FROM apple_transactions
WHERE original_transaction_id = $1;

-- name: InsertAppleTransaction :exec
INSERT INTO apple_transactions (original_transaction_id, user_id, product_id, tier)
VALUES ($1, $2, $3, $4);

-- name: ListAppleTransactionsByUser :many
SELECT original_transaction_id, user_id, product_id, tier, redeemed_at
FROM apple_transactions
WHERE user_id = $1
ORDER BY redeemed_at DESC;
