-- name: CreateFaiPaymentIntent :exec
INSERT INTO fai_payment_intents (
    id, user_id, payment_id, product_id, price_usd, fai_price, status
) VALUES ($1, $2, $3, $4, $5, $6, 'pending');

-- name: GetFaiPaymentIntentByPaymentID :one
SELECT * FROM fai_payment_intents WHERE payment_id = $1;

-- name: GetFaiPaymentIntentForUser :one
SELECT * FROM fai_payment_intents WHERE payment_id = $1 AND user_id = $2;

-- name: UpdateFaiPaymentIntentToCompleted :exec
UPDATE fai_payment_intents
SET status = 'completed',
    token_address = $2,
    token_amount = $3,
    paid_block = $4,
    tx_hash = $5,
    paid_at = NOW(),
    updated_at = NOW()
WHERE payment_id = $1 AND status = 'pending';

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
