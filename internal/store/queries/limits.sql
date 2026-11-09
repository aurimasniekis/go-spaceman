-- name: GetSpaceLimits :one
SELECT * FROM space_limits WHERE space_id = ?;

-- name: InsertSpaceLimits :exec
INSERT INTO space_limits (space_id, values_json, on_lower, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSpaceLimits :execrows
UPDATE space_limits SET values_json = sqlc.arg(values_json), on_lower = sqlc.arg(on_lower), updated_at = sqlc.arg(now),
  updated_by = sqlc.arg(by), version = version + 1
WHERE space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- Live runs are those whose observed state is not final (exited, failed, lost): exactly the runs spaceman_runs_list
-- shows (fleet.Live). desired is not consulted: a run that ended on its own keeps desired = 'running', and counting it
-- held its slot until an operator stopped it.
-- name: ListLiveWorkRunsInSpace :many
SELECT id, kind, role, observed FROM runs WHERE space_id = ? AND kind IN ('worker', 'diagnostic')
  AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown')
ORDER BY created_at, id;

-- name: ListLiveWorkRunsOnNode :many
SELECT id, space_id, kind, role, observed FROM runs WHERE node_id = ? AND kind IN ('worker', 'diagnostic')
  AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown')
ORDER BY created_at, id;

-- name: ListActiveWorkRunsInSpace :many
SELECT * FROM runs WHERE space_id = ? AND kind IN ('worker', 'diagnostic') AND desired = 'running'
  AND observed IN ('pending', 'starting', 'running', 'unknown')
ORDER BY created_at DESC, id DESC;

-- name: GetNodeCapacity :one
SELECT capacity FROM nodes WHERE id = ?;

-- name: GetRunToolCalls :one
SELECT * FROM run_tool_calls WHERE run_id = ?;

-- name: CountRunToolCall :exec
INSERT INTO run_tool_calls (run_id, space_id, calls, refused, updated_at) VALUES (?, ?, sqlc.arg(calls), sqlc.arg(refused), sqlc.arg(now))
ON CONFLICT (run_id) DO UPDATE SET calls = run_tool_calls.calls + excluded.calls, refused = run_tool_calls.refused + excluded.refused,
  updated_at = excluded.updated_at;

-- name: ListSpaceUsageSince :many
SELECT r.id AS run_id, r.created_at, COALESCE(u.source, '') AS source, COALESCE(u.models_json, '[]') AS models_json,
  COALESCE(u.estimate_json, '') AS estimate_json
FROM runs r LEFT JOIN run_usage u ON u.run_id = r.id
WHERE r.space_id = ? AND r.created_at >= ?;

-- name: InsertBudgetNotice :exec
INSERT INTO budget_notices (id, space_id, scope, run_id, budget, level, used, limit_value, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: LastBudgetNotice :one
SELECT * FROM budget_notices WHERE space_id = ? AND scope = ? AND run_id = ? AND budget = ? AND level = ? ORDER BY at DESC LIMIT 1;

-- name: ListRunningTopLevelTasks :many
SELECT t.id, t.space_id, t.title, t.started_at FROM tasks t
WHERE t.status = 'running' AND t.parent_id IS NULL AND t.started_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task_deadline_notices n WHERE n.task_id = t.id AND n.started_at = t.started_at);

-- name: InsertTaskDeadlineNotice :execrows
INSERT INTO task_deadline_notices (task_id, started_at, space_id, at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING;

-- name: ListSpacesWithLimits :many
SELECT space_id, values_json FROM space_limits;
