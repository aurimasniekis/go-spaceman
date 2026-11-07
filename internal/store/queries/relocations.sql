-- Space relocation (internal/fleet relocation.go, ADR-0037).

-- name: InsertRelocation :exec
INSERT INTO relocations (id, space_id, source_node_id, dest_node_id, mode, state, dirty_policy, stop_runs, recovery, retain_days,
  acknowledgement, repos_json, loss_json, generation_before, paused_space, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetRelocation :one
SELECT * FROM relocations WHERE id = ?;

-- name: GetActiveRelocation :one
SELECT * FROM relocations WHERE space_id = ? AND state NOT IN ('done', 'checked', 'failed', 'cancelled');

-- name: GetLatestRelocation :one
SELECT * FROM relocations WHERE space_id = ? ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: ListRelocationsBySpace :many
SELECT * FROM relocations WHERE space_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- name: ListActiveRelocations :many
SELECT * FROM relocations WHERE state NOT IN ('done', 'checked', 'failed', 'cancelled') ORDER BY created_at;

-- name: ListActiveRelocationsByNode :many
SELECT * FROM relocations WHERE state NOT IN ('done', 'checked', 'failed', 'cancelled') AND (source_node_id = sqlc.arg(node_id) OR dest_node_id = sqlc.arg(node_id))
ORDER BY created_at;

-- name: UpdateRelocationStep :execrows
UPDATE relocations SET state = sqlc.arg(state), blocked_step = sqlc.arg(blocked_step), step_command_id = sqlc.arg(step_command_id),
  error = sqlc.arg(error), version = version + 1, updated_at = sqlc.arg(now), finished_at = sqlc.narg(finished_at)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: SetRelocationReport :exec
UPDATE relocations SET report_json = ?, issues_json = ? WHERE id = ?;

-- name: SetRelocationExport :exec
UPDATE relocations SET export_json = ? WHERE id = ?;

-- name: SetRelocationPrepare :exec
UPDATE relocations SET prepare_json = ? WHERE id = ?;

-- name: SetRelocationPolicy :exec
UPDATE relocations SET dirty_policy = ? WHERE id = ?;

-- name: SetRelocationFinal :exec
UPDATE relocations SET generation_after = sqlc.arg(generation_after), source_state = sqlc.arg(source_state),
  release_command_id = sqlc.arg(release_command_id), retain_until = sqlc.arg(retain_until)
WHERE id = sqlc.arg(id);

-- name: SetRelocationSource :exec
UPDATE relocations SET source_state = sqlc.arg(source_state), release_command_id = sqlc.arg(release_command_id),
  retain_until = sqlc.arg(retain_until), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: NextRelocationRetention :one
SELECT CAST(coalesce(min(retain_until), 0) AS INTEGER) AS at FROM relocations WHERE source_state = 'retained' AND retain_until IS NOT NULL;

-- name: ListDueRelocationRetention :many
SELECT * FROM relocations WHERE source_state = 'retained' AND retain_until IS NOT NULL AND retain_until <= ? ORDER BY retain_until;

-- name: GetRelocationByCommand :one
SELECT * FROM relocations WHERE step_command_id = sqlc.arg(command_id) OR release_command_id = sqlc.arg(command_id);

-- name: SetSpaceDesiredNode :execrows
UPDATE spaces SET desired_node_id = sqlc.arg(desired_node_id), version = version + 1, updated_at = sqlc.arg(now) WHERE id = sqlc.arg(id);

-- name: FinalizeSpaceNode :execrows
UPDATE spaces SET node_id = sqlc.arg(node_id), desired_node_id = NULL, version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND desired_node_id = sqlc.arg(node_id);

-- name: MoveLease :exec
INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES (?, ?, ?, 'active', ?, ?, ?)
ON CONFLICT (space_id) DO UPDATE SET node_id = excluded.node_id, generation = excluded.generation, state = 'active',
  granted_at = excluded.granted_at, renewed_at = excluded.renewed_at, expires_at = excluded.expires_at;

-- name: CancelSpaceNodeCommands :execrows
UPDATE node_commands SET state = 'cancelled', reason = sqlc.arg(reason)
WHERE space_id = sqlc.arg(space_id) AND node_id = sqlc.arg(node_id) AND state IN ('pending', 'sent') AND kind NOT LIKE 'space.relocate.%';

-- name: MarkRunsLostForRelocation :execrows
UPDATE runs SET observed = 'lost', desired = 'stopped', end_reason = 'relocated_source_unavailable',
  ended_at = coalesce(ended_at, sqlc.arg(now)), version = version + 1, updated_at = sqlc.arg(now)
WHERE space_id = sqlc.arg(space_id) AND node_id = sqlc.arg(node_id) AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown');

-- name: RevokeSpaceRunCredentials :execrows
UPDATE run_credentials SET revoked_at = sqlc.arg(revoked_at)
WHERE revoked_at IS NULL AND run_id IN (SELECT id FROM runs WHERE space_id = sqlc.arg(space_id));

-- name: CountRunsWithSession :one
SELECT CAST(count(*) AS INTEGER) AS total, CAST(coalesce(sum(CASE WHEN node_id = sqlc.arg(node_id) THEN 1 ELSE 0 END), 0) AS INTEGER) AS on_node
FROM runs WHERE space_id = sqlc.arg(space_id) AND session_id = sqlc.arg(session_id);

-- name: ListRunsLostForRelocation :many
SELECT * FROM runs
WHERE space_id = sqlc.arg(space_id) AND node_id = sqlc.arg(node_id) AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown')
ORDER BY id;
