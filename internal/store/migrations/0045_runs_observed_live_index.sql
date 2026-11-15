-- 0045_runs_observed_live_index: a partial index over live runs (SM-OBS-006, ADR-0067). The System page's
-- runs_running count (queries/system.sql) otherwise scans an index over every run ever recorded; with this index it
-- reads only the runs that are starting, running or stopping. The WHERE clause must match the query's exactly for
-- SQLite to use it.

-- +goose Up
CREATE INDEX ix_runs_observed_live ON runs (observed) WHERE observed IN ('starting', 'running', 'stopping');

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
