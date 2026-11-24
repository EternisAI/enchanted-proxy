-- name: TryLockEntitlementReview :one
-- Takes the entitlement review lock until the transaction ends, if no other
-- server instance holds it.
SELECT pg_try_advisory_xact_lock(hashtextextended('entitlement_review', 0)) AS locked;

-- name: HasEnforcedEntitlementReview :one
-- Whether an enforced run has finished without errors. A run that hit an error
-- reset nothing, so the review may run again.
SELECT EXISTS (
    SELECT 1 FROM entitlement_review_runs
    WHERE mode = 'enforce' AND finished_at IS NOT NULL AND report->'errors' IS NULL
) AS enforced;

-- name: StartEntitlementReviewRun :one
INSERT INTO entitlement_review_runs (mode)
VALUES (sqlc.arg(mode))
RETURNING id;

-- name: FinishEntitlementReviewRun :exec
-- The run is started and finished in one transaction, whose NOW() is its
-- start, so the finish takes the wall clock.
UPDATE entitlement_review_runs SET finished_at = clock_timestamp(), report = $2 WHERE id = $1;

-- name: ListUnrecordedApplePlusEntitlements :many
-- Active Apple Plus entitlements with no purchase on record.
SELECT user_id
FROM entitlements
WHERE subscription_provider = 'apple'
  AND subscription_tier = 'plus'
  AND apple_original_transaction_id IS NULL
  AND (subscription_expires_at IS NULL OR subscription_expires_at > NOW())
ORDER BY user_id;

-- name: BackupEntitlements :execrows
INSERT INTO entitlements_review_backup (
    run_id, user_id, subscription_tier, subscription_expires_at, subscription_provider, stripe_customer_id, updated_at
)
SELECT sqlc.arg(run_id)::bigint, user_id, subscription_tier, subscription_expires_at, subscription_provider, stripe_customer_id, updated_at
FROM entitlements;

-- name: ResetUnrecordedApplePlusEntitlements :many
-- Sets the given accounts to Free, each only if its entitlement is still an
-- Apple Plus entitlement with no purchase on record.
UPDATE entitlements
SET subscription_tier = 'free', subscription_expires_at = NULL, updated_at = NOW()
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND subscription_provider = 'apple'
  AND subscription_tier = 'plus'
  AND apple_original_transaction_id IS NULL
RETURNING user_id;
