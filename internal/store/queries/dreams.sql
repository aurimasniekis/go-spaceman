-- name: InsertDreamConfig :exec
INSERT INTO dream_configs (id, space_id, slug, name, space_role, instance_id, memory_roles_json, transcript_roles_json, include_own, instructions,
  interval_hours, max_minutes, max_tokens, max_files, max_bytes, enabled, next_due_at, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateDreamConfig :execrows
UPDATE dream_configs SET name = sqlc.arg(name), space_role = sqlc.arg(space_role), memory_roles_json = sqlc.arg(memory_roles_json),
  transcript_roles_json = sqlc.arg(transcript_roles_json), include_own = sqlc.arg(include_own), instructions = sqlc.arg(instructions),
  interval_hours = sqlc.arg(interval_hours), max_minutes = sqlc.arg(max_minutes), max_tokens = sqlc.arg(max_tokens), max_files = sqlc.arg(max_files),
  max_bytes = sqlc.arg(max_bytes), enabled = sqlc.arg(enabled), next_due_at = sqlc.arg(next_due_at), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetDreamConfig :one
SELECT * FROM dream_configs WHERE id = ?;

-- name: ListDreamConfigs :many
SELECT * FROM dream_configs WHERE space_id = ? ORDER BY slug;

-- name: ListDueDreamConfigs :many
SELECT * FROM dream_configs WHERE enabled = 1 AND next_due_at IS NOT NULL ORDER BY next_due_at;

-- name: SetDreamNextDue :exec
UPDATE dream_configs SET next_due_at = ?, updated_at = ? WHERE id = ?;

-- name: InsertDreamRun :exec
INSERT INTO dream_runs (id, config_id, space_id, run_id, trigger_kind, state, started_at, deadline_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'running', ?, ?, ?);

-- name: GetDreamRun :one
SELECT * FROM dream_runs WHERE id = ?;

-- name: GetDreamRunByRun :one
SELECT * FROM dream_runs WHERE run_id = ? AND run_id != '';

-- name: GetDreamRunByCommand :one
SELECT * FROM dream_runs WHERE commit_command_id = ? AND commit_command_id != '';

-- name: ListDreamRuns :many
SELECT * FROM dream_runs WHERE config_id = ? ORDER BY started_at DESC LIMIT ?;

-- name: ListActiveDreamRuns :many
SELECT * FROM dream_runs WHERE state IN ('running', 'reported') ORDER BY deadline_at;

-- name: CountActiveDreamRuns :one
SELECT COUNT(*) FROM dream_runs WHERE config_id = ? AND state IN ('running', 'reported');

-- name: ReportDreamRun :execrows
UPDATE dream_runs SET state = 'reported', summary = ?, findings_json = ?, no_change = ?, commit_command_id = ?, updated_at = ?
WHERE id = ? AND state = 'running';

-- name: EndDreamRun :execrows
UPDATE dream_runs SET state = sqlc.arg(state), detail = sqlc.arg(detail), ended_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('running', 'reported');
