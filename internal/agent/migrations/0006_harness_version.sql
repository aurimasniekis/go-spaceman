-- 0006_harness_version: the harness version each run launched (ADR-0077). A managed Claude Code run records the cached
-- version it runs, its binary's absolute path, what was requested (a channel or X.Y.Z) and why another version ran
-- instead (a dist fallback reason, '' when the target ran). The version of a run that is live or adoptable is kept out of
-- the cache's garbage collection; an adopted run reports what it started with.

-- +goose Up
ALTER TABLE runs ADD COLUMN harness_version TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_path TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_requested TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN harness_fallback TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE runs DROP COLUMN harness_fallback;
ALTER TABLE runs DROP COLUMN harness_requested;
ALTER TABLE runs DROP COLUMN harness_path;
ALTER TABLE runs DROP COLUMN harness_version;
