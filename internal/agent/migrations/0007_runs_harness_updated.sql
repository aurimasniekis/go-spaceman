-- 0007_runs_harness_updated: the Claude Code cache's GC reads the exact versions profiles pinned on the runs of one
-- harness updated within the last 30 days (harnessRequestsSince, PR #43 review N3). The journal keeps every run, so
-- without an index that query scans the whole table on every GC.

-- +goose Up
CREATE INDEX ix_runs_harness_updated ON runs (harness, updated_at);

-- +goose Down
DROP INDEX ix_runs_harness_updated;
