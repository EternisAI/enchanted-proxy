-- name: TryLockTaskReconciler :one
-- Takes the reconciler's lock until the transaction ends, if no other server
-- instance holds it.
SELECT pg_try_advisory_xact_lock(hashtextextended('task_reconciler', 0))::boolean AS locked;

-- name: RecentTaskReconcilerRun :one
-- Whether a sweep that finished started within the last given seconds, by the
-- database's clock.
SELECT EXISTS (
    SELECT 1 FROM task_reconciler_runs
    WHERE finished_at IS NOT NULL
      AND started_at > NOW() - make_interval(secs => sqlc.arg(seconds)::float8)
)::boolean AS recent;

-- name: StartTaskReconcilerRun :one
INSERT INTO task_reconciler_runs (mode) VALUES ($1) RETURNING id;

-- name: FinishTaskReconcilerRun :exec
-- The run is started and finished in one transaction, whose NOW() is its
-- start, so the finish takes the wall clock.
UPDATE task_reconciler_runs SET finished_at = clock_timestamp(), report = $2 WHERE id = $1;

-- name: ListTasksToReconcile :many
-- Tasks that may still run or be resumed, oldest first.
SELECT * FROM tasks
WHERE status IN ('pending', 'active', 'paused')
ORDER BY created_at, task_id;

-- name: ActivatePendingTask :execrows
-- A task whose schedule exists although its creation did not finish.
UPDATE tasks SET status = 'active', updated_at = NOW()
WHERE task_id = $1 AND status = 'pending';

-- name: CompleteOneTimeTask :execrows
UPDATE tasks SET status = 'completed', updated_at = NOW()
WHERE task_id = $1 AND type = 'one_time' AND status IN ('pending', 'active');

-- name: UpdateTaskPauseReason :execrows
UPDATE tasks SET pause_reason = sqlc.arg(pause_reason), updated_at = NOW()
WHERE task_id = sqlc.arg(task_id) AND status = 'paused';

-- name: DeleteTaskByID :execrows
DELETE FROM tasks WHERE task_id = $1;

-- name: MarkTaskOverLimit :exec
-- Records when the task was first found over its owner's plan; a later call
-- keeps the first time.
UPDATE tasks SET over_limit_since = COALESCE(over_limit_since, sqlc.arg(since))
WHERE task_id = sqlc.arg(task_id);

-- name: ClearTaskOverLimit :exec
UPDATE tasks SET over_limit_since = NULL WHERE task_id = $1 AND over_limit_since IS NOT NULL;
