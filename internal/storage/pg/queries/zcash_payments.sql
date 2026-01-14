-- name: GetZcashPayment :one
SELECT invoice_id, user_id, product_id, amount_zat, redeemed_at
FROM zcash_payments
WHERE invoice_id = $1;

-- name: InsertZcashPayment :exec
INSERT INTO zcash_payments (invoice_id, user_id, product_id, amount_zat)
VALUES ($1, $2, $3, $4);

-- name: ListZcashPaymentsByUser :many
SELECT invoice_id, user_id, product_id, amount_zat, redeemed_at
FROM zcash_payments
WHERE user_id = $1
ORDER BY redeemed_at DESC;
