-- name: CreateTask :one
INSERT INTO tasks (user_id, chat_id, name, content, cron, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
RETURNING *;

-- name: GetTasksByUserID :many
SELECT * FROM tasks
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: GetTasksByChatID :many
SELECT * FROM tasks
WHERE chat_id = $1
ORDER BY created_at DESC;

-- name: GetTaskByID :one
SELECT * FROM tasks
WHERE id = $1;

-- name: DeleteTask :exec
DELETE FROM tasks
WHERE id = $1;

-- name: DeleteTaskByIDAndChatID :exec
DELETE FROM tasks
WHERE id = $1 AND chat_id = $2;

-- name: DeleteTaskByIDAndUserID :exec
DELETE FROM tasks
WHERE id = $1 AND user_id = $2;

-- name: UpdateTask :one
UPDATE tasks
SET name = $2, content = $3, cron = $4, updated_at = NOW()
WHERE id = $1
RETURNING *;
