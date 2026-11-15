-- 0008_runs_limit: a run's usage limit as the node last detected it (ADR-0079, node protocol minor 12). limit_kind ''
-- means none; limit_resets_at NULL is an unknown reset; limit_since is when it was first seen. limit_offset is how far
-- the run's transcript has been scanned, so a restarted agent resumes the scan and re-reports the same limit instead of
-- reading the transcript from the start. Only typed fields are kept, never transcript text.

-- +goose Up
ALTER TABLE runs ADD COLUMN limit_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN limit_resets_at INTEGER;
ALTER TABLE runs ADD COLUMN limit_since INTEGER;
ALTER TABLE runs ADD COLUMN limit_offset INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE runs DROP COLUMN limit_offset;
ALTER TABLE runs DROP COLUMN limit_since;
ALTER TABLE runs DROP COLUMN limit_resets_at;
ALTER TABLE runs DROP COLUMN limit_kind;
