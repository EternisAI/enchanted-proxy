-- +goose Up
-- When the task reconciler first found a task over the limits of its owner's
-- current plan. A task still over once this is older than the grace period is
-- held (paused with reason plan_ended). NULL while the task fits.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS over_limit_since TIMESTAMPTZ;

-- One row per reconciler sweep: when it ran, in which mode, and what it did or
-- would do (counts and task IDs per action). It lets replicas skip a sweep
-- another one has just made, and keeps the reports readable from SQL.
CREATE TABLE IF NOT EXISTS task_reconciler_runs (
    id          BIGSERIAL PRIMARY KEY,
    started_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at TIMESTAMPTZ,
    mode        TEXT NOT NULL,
    report      JSONB
);

CREATE INDEX IF NOT EXISTS idx_task_reconciler_runs_started ON task_reconciler_runs (started_at);

-- +goose Down
DROP TABLE IF EXISTS task_reconciler_runs;
ALTER TABLE tasks DROP COLUMN IF EXISTS over_limit_since;
