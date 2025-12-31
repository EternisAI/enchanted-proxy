-- name: CreateProblemReport :one
INSERT INTO problem_reports (
    id, user_id, problem_description,
    device_model, device_name, system_name, system_version,
    app_version, build_number, locale, timezone,
    total_capacity_bytes, available_capacity_bytes, used_capacity_bytes,
    subscription_tier, contact_email, parent_id, ticket_id, embedding,
    created_at, updated_at
)
VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14,
    $15, $16, $17, $18, $19,
    NOW(), NOW()
)
RETURNING *;

-- name: CountProblemReportsByUserID :one
SELECT COUNT(*) FROM problem_reports
WHERE user_id = $1;

-- name: FindSimilarProblemReports :many
SELECT id, user_id, problem_description, parent_id, ticket_id, created_at,
       (1 - (embedding <=> $1::vector))::float8 AS similarity
FROM problem_reports
WHERE parent_id IS NULL
  AND embedding IS NOT NULL
ORDER BY embedding <=> $1::vector
LIMIT 5;

-- name: GetProblemReportByID :one
SELECT * FROM problem_reports
WHERE id = $1;

-- name: UpdateProblemReportTicketID :exec
UPDATE problem_reports
SET ticket_id = $2, updated_at = NOW()
WHERE id = $1;

-- name: GetParentProblemReport :one
SELECT * FROM problem_reports
WHERE id = $1 AND parent_id IS NULL;
