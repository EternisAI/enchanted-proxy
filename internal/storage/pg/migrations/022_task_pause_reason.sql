-- +goose Up
-- Why a task is paused: inactive, account_missing, plan_ended,
-- over_allowance, user or admin. NULL while the task is not paused.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pause_reason TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN IF EXISTS pause_reason;
