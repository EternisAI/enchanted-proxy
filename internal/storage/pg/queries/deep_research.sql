-- name: CreateDeepResearchRun :one
INSERT INTO deep_research_runs (user_id, chat_id, run_date, status)
VALUES ($1, $2, CURRENT_DATE, 'active')
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

-- name: GetUserDeepResearchRunsToday :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = $1
  AND run_date = CURRENT_DATE
  AND status IN ('completed', 'active');

-- name: GetUserDeepResearchRunsLifetime :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = $1
  AND status IN ('completed', 'active');

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
    WHERE user_id = $1
      AND status = 'active'
) as has_active;

-- name: GetDeepResearchRunCountForChat :one
SELECT COUNT(*) as run_count
FROM deep_research_runs
WHERE user_id = $1
  AND chat_id = $2
  AND status IN ('completed', 'active');
