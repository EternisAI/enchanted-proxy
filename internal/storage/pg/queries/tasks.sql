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

-- name: LockTaskAdmission :exec
-- Holds an account's task admission lock until the transaction ends, so the
-- limit checks and the new task's rows cannot interleave with another
-- admission for the same account.
SELECT pg_advisory_xact_lock(hashtextextended('task_admission:' || sqlc.arg(user_id)::text, 0));

-- name: DeleteTaskCreation :exec
-- Releases the creation recorded for a task whose schedule could not be set up.
DELETE FROM task_creations WHERE task_id = $1;

-- name: GetTaskForUser :one
SELECT * FROM tasks
WHERE task_id = $1 AND user_id = $2;

-- name: MarkTaskPaused :execrows
-- Records that a task's schedule is paused, and why. A task that is already
-- paused keeps its reason, and a finished one is left as it is.
UPDATE tasks
SET status = 'paused', pause_reason = sqlc.arg(pause_reason), updated_at = NOW()
WHERE task_id = sqlc.arg(task_id) AND status IN ('pending', 'active');

-- name: MarkTaskResumed :execrows
-- Records that a paused task's schedule runs again. Only a task still paused
-- for the reason the caller checked is changed.
UPDATE tasks
SET status = 'active', pause_reason = NULL, updated_at = NOW()
WHERE task_id = sqlc.arg(task_id) AND status = 'paused'
  AND pause_reason IS NOT DISTINCT FROM sqlc.narg(pause_reason);

-- name: LockTask :exec
-- Holds one task's lock until the transaction ends, so creating, pausing,
-- resuming and deleting the same task cannot interleave.
SELECT pg_advisory_xact_lock(hashtextextended('task:' || sqlc.arg(task_id)::text, 0));
