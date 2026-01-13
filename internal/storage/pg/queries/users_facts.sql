-- name: CreateUserFact :one
INSERT INTO users_facts (id, user_id, fact_body, fact_type, created_at)
VALUES ($1, $2, $3, $4, NOW())
RETURNING *;

-- name: GetUserFactByID :one
SELECT * FROM users_facts
WHERE id = $1;

-- name: GetUserFactsByUserID :many
SELECT * FROM users_facts
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: GetUserFactsByUserIDAndType :many
SELECT * FROM users_facts
WHERE user_id = $1 AND fact_type = $2
ORDER BY created_at DESC;

-- name: DeleteUserFact :exec
DELETE FROM users_facts
WHERE id = $1 AND user_id = $2;

-- name: DeleteUserFactsByUserID :exec
DELETE FROM users_facts
WHERE user_id = $1;
