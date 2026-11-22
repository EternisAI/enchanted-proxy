-- name: CreateFaiPaymentIntent :exec
INSERT INTO fai_payment_intents (
    id, user_id, payment_id, product_id, price_usd, fai_price, status
) VALUES ($1, $2, $3, $4, $5, $6, 'pending');

-- name: GetFaiPaymentIntentByPaymentID :one
SELECT * FROM fai_payment_intents WHERE payment_id = $1;

-- name: GetFaiPaymentIntentForUser :one
SELECT * FROM fai_payment_intents WHERE payment_id = $1 AND user_id = $2;

-- name: UpdateFaiPaymentIntentToCompleted :execrows
-- Claims an intent. Each payment event is delivered to every replica and again by
-- the historical sync, so the caller grants only when this changed a row. An
-- expired intent is claimed only when allow_expired is set, which the caller does
-- after checking that the payment was made before the intent expired.
UPDATE fai_payment_intents
SET status = 'completed',
    token_address = sqlc.arg(token_address),
    token_amount = sqlc.arg(token_amount),
    paid_block = sqlc.arg(paid_block),
    tx_hash = sqlc.arg(tx_hash),
    paid_at = NOW(),
    updated_at = NOW()
WHERE payment_id = sqlc.arg(payment_id)
  AND (status = 'pending' OR (status = 'expired' AND sqlc.arg(allow_expired)::bool));

-- name: UpdateFaiPaymentIntentToExpired :exec
UPDATE fai_payment_intents
SET status = 'expired', updated_at = NOW()
WHERE id = $1 AND status = 'pending';

-- name: GetExpiredPendingFaiPaymentIntents :many
SELECT * FROM fai_payment_intents
WHERE status = 'pending'
  AND created_at < NOW() - INTERVAL '24 hours'
ORDER BY created_at ASC
LIMIT $1;
