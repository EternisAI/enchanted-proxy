-- name: CreateTask :one
INSERT INTO tasks (task_id, user_id, chat_id, task_name, task_text, type, time, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW(), NOW())
RETURNING *;

-- name: GetTaskByID :one
SELECT * FROM tasks
WHERE task_id = $1;

-- name: GetTasksByUserID :many
SELECT * FROM tasks
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: GetTasksByChatID :many
SELECT * FROM tasks
WHERE chat_id = $1
ORDER BY created_at DESC;

-- name: UpdateTaskStatus :exec
UPDATE tasks
SET status = $2, updated_at = NOW()
WHERE task_id = $1;

-- name: DeleteTask :exec
DELETE FROM tasks
WHERE task_id = $1;

-- name: GetAllActiveTasks :many
SELECT * FROM tasks
WHERE status = 'active'
ORDER BY created_at DESC;
