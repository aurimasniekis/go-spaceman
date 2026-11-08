-- System status (SM-OBS-006): installation-wide counts for the admin System page, one query.

-- name: SystemCounts :one
SELECT
  CAST((SELECT count(*) FROM spaces WHERE state != 'archived') AS INTEGER) AS spaces,
  CAST((SELECT count(*) FROM runs WHERE observed IN ('starting', 'running', 'stopping')) AS INTEGER) AS runs_running,
  CAST((SELECT count(*) FROM human_actions WHERE status IN ('open', 'in_progress')) AS INTEGER) AS human_pending;
