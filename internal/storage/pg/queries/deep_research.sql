-- name: CreateDeepResearchRun :one
INSERT INTO deep_research_runs (user_id, chat_id, run_date, status, tier)
VALUES ($1, $2, CURRENT_DATE, 'active', $3)
RETURNING id;

-- name: UpdateDeepResearchRunTokens :exec
UPDATE deep_research_runs
SET model_tokens_used = $2,
    plan_tokens_used = $3
WHERE id = $1;

-- name: CompleteDeepResearchRun :exec
UPDATE deep_research_runs
SET status = $2,
    completed_at = NOW()
WHERE id = $1;

-- A run counts toward the allowance whatever its outcome, unless it failed within
-- a minute of starting (the backend refused or broke before doing work).

-- name: GetUserDeepResearchRunsToday :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND run_date = CURRENT_DATE
  AND NOT (status = 'failed' AND completed_at IS NOT NULL AND completed_at < started_at + INTERVAL '60 seconds');

-- name: GetUserDeepResearchRunsThisWeek :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND run_date >= DATE_TRUNC('week', CURRENT_DATE)::date
  AND NOT (status = 'failed' AND completed_at IS NOT NULL AND completed_at < started_at + INTERVAL '60 seconds');

-- name: GetUserDeepResearchRunsLifetime :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
  AND NOT (status = 'failed' AND completed_at IS NOT NULL AND completed_at < started_at + INTERVAL '60 seconds');

-- name: CountDeepResearchRunsToday :one
-- All accounts, for the service-wide daily ceiling.
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE run_date = CURRENT_DATE
  AND NOT (status = 'failed' AND completed_at IS NOT NULL AND completed_at < started_at + INTERVAL '60 seconds');

-- name: GetActiveDeepResearchRun :one
SELECT id, model_tokens_used
FROM deep_research_runs
WHERE user_id = $1
  AND chat_id = $2
  AND status = 'active'
ORDER BY started_at DESC
LIMIT 1;

-- name: HasActiveDeepResearchRun :one
SELECT EXISTS(
    SELECT 1
    FROM deep_research_runs
    WHERE user_id = ANY(sqlc.arg(user_ids)::text[])
      AND status = 'active'
) as has_active;

-- name: FailStaleDeepResearchRuns :execrows
-- Runs still active long after any run could have finished: their connection
-- was lost without the run being completed (for example a restart).
UPDATE deep_research_runs
SET status = 'failed', completed_at = NOW()
WHERE status = 'active'
  AND started_at < sqlc.arg(started_before);

-- name: GetDeepResearchRunCountForChat :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = $1
  AND chat_id = $2
  AND status IN ('completed', 'active');
