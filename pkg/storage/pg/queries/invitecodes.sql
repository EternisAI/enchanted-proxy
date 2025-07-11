-- name: CreateInviteCode :one
INSERT INTO invite_codes (code, code_hash, bound_email, created_by, is_used, redeemed_by, redeemed_at, expires_at, is_active, created_at, updated_at) 
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW()) 
RETURNING *;

-- name: GetAllInviteCodes :many
SELECT * FROM invite_codes 
WHERE deleted_at IS NULL 
ORDER BY created_at DESC;

-- name: GetInviteCodeByCodeHash :one
SELECT * FROM invite_codes 
WHERE code_hash = $1 AND deleted_at IS NULL;

-- name: GetInviteCodeByID :one
SELECT * FROM invite_codes 
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateInviteCodeUsage :exec
UPDATE invite_codes 
SET is_used = $2, redeemed_by = $3, redeemed_at = $4, updated_at = NOW() 
WHERE id = $1;

-- name: AtomicUseInviteCode :exec
UPDATE invite_codes 
SET is_used = true, redeemed_by = $2, redeemed_at = $3, updated_at = NOW() 
WHERE code_hash = $1 
  AND deleted_at IS NULL 
  AND is_active = true 
  AND is_used = false 
  AND (expires_at IS NULL OR expires_at > NOW())
  AND (bound_email IS NULL OR bound_email = $4);

-- name: SoftDeleteInviteCode :exec
UPDATE invite_codes 
SET deleted_at = NOW(), updated_at = NOW() 
WHERE id = $1;

-- name: UpdateInviteCodeActive :exec
UPDATE invite_codes 
SET is_active = $2, updated_at = NOW() 
WHERE id = $1;

-- name: CountInviteCodesByRedeemedBy :one
SELECT COUNT(*) FROM invite_codes 
WHERE redeemed_by = $1 AND deleted_at IS NULL;

-- name: ResetInviteCode :exec
UPDATE invite_codes 
SET is_used = false, redeemed_by = NULL, redeemed_at = NULL, updated_at = NOW() 
WHERE code_hash = $1 AND deleted_at IS NULL;