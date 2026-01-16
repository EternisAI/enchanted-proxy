-- name: CreateZcashInvoice :exec
INSERT INTO zcash_invoices (
    id, user_id, product_id, amount_zatoshis, zec_amount,
    price_usd, receiving_address, status
) VALUES ($1, $2, $3, $4, $5, $6, $7, 'pending');

-- name: GetZcashInvoice :one
SELECT * FROM zcash_invoices WHERE id = $1;

-- name: GetZcashInvoiceForUser :one
SELECT * FROM zcash_invoices WHERE id = $1 AND user_id = $2;

-- name: GetZcashInvoicesByUserAndStatus :many
SELECT * FROM zcash_invoices
WHERE user_id = $1 AND status = $2
ORDER BY created_at DESC;

-- name: UpdateZcashInvoiceStatus :exec
UPDATE zcash_invoices
SET status = $2, updated_at = NOW()
WHERE id = $1;

-- name: UpdateZcashInvoiceToProcessing :exec
UPDATE zcash_invoices
SET status = 'processing', updated_at = NOW()
WHERE id = $1 AND status = 'pending';

-- name: UpdateZcashInvoiceToPaid :exec
UPDATE zcash_invoices
SET status = 'paid', paid_at = NOW(), updated_at = NOW()
WHERE id = $1 AND status IN ('pending', 'processing');

-- name: GetExpiredPendingInvoices :many
SELECT * FROM zcash_invoices
WHERE status = 'pending'
  AND created_at < NOW() - INTERVAL '24 hours'
ORDER BY created_at ASC
LIMIT $1;

-- name: UpdateZcashInvoiceToExpired :exec
UPDATE zcash_invoices
SET status = 'expired', updated_at = NOW()
WHERE id = $1 AND status = 'pending';
