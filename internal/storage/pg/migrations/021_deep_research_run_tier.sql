-- +goose Up
-- The plan the account was on when the run started, so runs per plan can be
-- watched. NULL for runs started before this column existed.
ALTER TABLE deep_research_runs ADD COLUMN IF NOT EXISTS tier TEXT;

-- +goose Down
ALTER TABLE deep_research_runs DROP COLUMN IF EXISTS tier;
