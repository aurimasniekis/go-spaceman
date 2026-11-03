-- name: InsertTask :exec
INSERT INTO tasks (id, space_id, parent_id, title, objective, acceptance, kind, priority, rank, status, exclusions_json, assignee_role,
  workspace, origin_kind, origin_id, dedup_key, created_at, created_by, updated_at, depth)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetTask :one
SELECT * FROM tasks WHERE id = ?;

-- name: GetTaskByDedup :one
SELECT * FROM tasks WHERE space_id = ? AND dedup_key = ?;

-- name: ListTasks :many
SELECT * FROM tasks WHERE space_id = ? ORDER BY rank, created_at;

-- name: UpdateTaskFields :execrows
UPDATE tasks SET title = sqlc.arg(title), objective = sqlc.arg(objective), acceptance = sqlc.arg(acceptance), priority = sqlc.arg(priority),
  exclusions_json = sqlc.arg(exclusions_json), assignee_role = sqlc.arg(assignee_role), assignee_run = sqlc.arg(assignee_run),
  workspace = sqlc.arg(workspace), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: SetTaskStatus :execrows
UPDATE tasks SET status = sqlc.arg(status), status_reason = sqlc.arg(status_reason), result = sqlc.arg(result),
  preemptions = preemptions + sqlc.arg(preempted),
  started_at = CASE WHEN sqlc.arg(status) = 'running' AND started_at IS NULL THEN sqlc.arg(now) ELSE started_at END,
  finished_at = CASE WHEN sqlc.arg(mark_finished) = 1 THEN sqlc.arg(now) ELSE NULL END,
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: SetTaskRank :exec
UPDATE tasks SET rank = sqlc.arg(rank), manual_position = sqlc.arg(manual_position), version = version + sqlc.arg(bump), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: SetTaskIncident :exec
-- An incident's trigger, a proposal's source and the incident that preempted a task (SM-QUEUE-009).
UPDATE tasks SET kind = sqlc.arg(kind), incident_trigger = sqlc.arg(incident_trigger), incident_proposed_by = sqlc.arg(incident_proposed_by),
  version = version + sqlc.arg(bump), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: SetTaskPreemptedBy :exec
UPDATE tasks SET preempted_by = sqlc.arg(preempted_by) WHERE id = sqlc.arg(id);

-- name: InsertTaskPreemption :exec
INSERT INTO task_preemptions (id, space_id, incident_id, task_id, run_id, action, created_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListOpenTaskPreemptions :many
SELECT * FROM task_preemptions WHERE task_id = ? AND resumed_at IS NULL ORDER BY created_at, id;

-- name: ListIncidentPreemptions :many
SELECT * FROM task_preemptions WHERE incident_id = ? AND resumed_at IS NULL ORDER BY created_at, id;

-- name: ListTaskPreemptions :many
SELECT * FROM task_preemptions WHERE task_id = ? ORDER BY created_at DESC, id LIMIT 50;

-- name: CloseTaskPreemption :exec
UPDATE task_preemptions SET resumed_at = sqlc.arg(now), resumed_by = sqlc.arg(by), outcome = sqlc.arg(outcome) WHERE id = sqlc.arg(id) AND resumed_at IS NULL;

-- name: SetTaskRankOnly :exec
UPDATE tasks SET rank = ? WHERE id = ?;

-- name: DeleteTaskDependencies :exec
DELETE FROM task_dependencies WHERE task_id = ?;

-- name: InsertTaskDependency :exec
INSERT INTO task_dependencies (task_id, depends_on) VALUES (?, ?);

-- name: ListSpaceDependencies :many
SELECT d.task_id, d.depends_on FROM task_dependencies d JOIN tasks t ON t.id = d.task_id WHERE t.space_id = ?;

-- name: InsertTaskBlocker :exec
INSERT INTO task_blockers (id, task_id, kind, ref_id, note, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ResolveTaskBlocker :execrows
UPDATE task_blockers SET resolved_at = sqlc.arg(now), resolved_by = sqlc.arg(by) WHERE id = sqlc.arg(id) AND task_id = sqlc.arg(task_id) AND resolved_at IS NULL;

-- name: ResolveBlockersByRef :many
UPDATE task_blockers SET resolved_at = sqlc.arg(now), resolved_by = sqlc.arg(by) WHERE kind = sqlc.arg(kind) AND ref_id = sqlc.arg(ref_id) AND resolved_at IS NULL
RETURNING task_id;

-- name: ListSpaceBlockers :many
SELECT b.* FROM task_blockers b JOIN tasks t ON t.id = b.task_id WHERE t.space_id = ? ORDER BY b.created_at;

-- name: GetQueuePolicy :one
SELECT * FROM queue_policies WHERE space_id = ?;

-- name: UpsertQueuePolicy :exec
INSERT INTO queue_policies (space_id, max_active, incident_insertion, incident_preempts, escalation_reorders, incident_triggers_json,
  incident_untrusted, incident_workers, incident_capacity, incident_resume, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (space_id) DO UPDATE SET max_active = excluded.max_active, incident_insertion = excluded.incident_insertion,
  incident_preempts = excluded.incident_preempts, escalation_reorders = excluded.escalation_reorders,
  incident_triggers_json = excluded.incident_triggers_json, incident_untrusted = excluded.incident_untrusted,
  incident_workers = excluded.incident_workers, incident_capacity = excluded.incident_capacity, incident_resume = excluded.incident_resume,
  version = queue_policies.version + 1, updated_at = excluded.updated_at, updated_by = excluded.updated_by;

-- name: InsertQueueDecision :exec
INSERT INTO queue_decisions (space_id, selected_id, skipped_json, at, by) VALUES (?, ?, ?, ?, ?);

-- name: ListQueueDecisions :many
SELECT * FROM queue_decisions WHERE space_id = ? ORDER BY id DESC LIMIT ?;

-- name: PurgeQueueDecisions :execrows
DELETE FROM queue_decisions WHERE at < ?;
