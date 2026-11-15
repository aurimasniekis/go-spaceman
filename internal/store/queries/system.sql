-- System status (SM-OBS-006): installation-wide counts for the admin System page, one query. runs_running reads the
-- partial index ix_runs_observed_live (migration 0045): keep its IN list identical to the index's WHERE clause.

-- name: SystemCounts :one
SELECT
  CAST((SELECT count(*) FROM spaces WHERE state != 'archived') AS INTEGER) AS spaces,
  CAST((SELECT count(*) FROM runs WHERE observed IN ('starting', 'running', 'stopping')) AS INTEGER) AS runs_running,
  CAST((SELECT count(*) FROM human_actions WHERE status IN ('open', 'in_progress')) AS INTEGER) AS human_pending;
