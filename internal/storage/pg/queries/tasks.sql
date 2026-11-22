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

-- name: DeleteTask :execresult
DELETE FROM tasks
WHERE task_id = $1 AND user_id = $2;

-- name: GetAllActiveTasks :many
SELECT * FROM tasks
WHERE status = 'active'
ORDER BY created_at DESC;

-- name: ListOpenTasksForUsers :many
-- Tasks that may still run, for the limit checks: the caller's quota group.
SELECT task_id, type, time, status, created_at
FROM tasks
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND status IN ('pending', 'active');

-- name: RecordTaskCreation :exec
INSERT INTO task_creations (user_id, task_id) VALUES ($1, $2);

-- name: CountTaskCreationsSince :one
SELECT COUNT(*) FROM task_creations
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND created_at > sqlc.arg(since);
