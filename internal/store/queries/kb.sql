-- name: InsertKB :exec
INSERT INTO kb_repos (id, space_id, slug, name, description, repo, base_branch, space_role, entry_instructions, validator_json, write_policy,
  allowed_roles_json, max_sessions, idle_sleep_seconds, request_timeout_seconds, session_ttl_seconds, enabled, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateKB :execrows
UPDATE kb_repos SET name = sqlc.arg(name), description = sqlc.arg(description), repo = sqlc.arg(repo), base_branch = sqlc.arg(base_branch),
  space_role = sqlc.arg(space_role), entry_instructions = sqlc.arg(entry_instructions), validator_json = sqlc.arg(validator_json),
  write_policy = sqlc.arg(write_policy), allowed_roles_json = sqlc.arg(allowed_roles_json), max_sessions = sqlc.arg(max_sessions),
  idle_sleep_seconds = sqlc.arg(idle_sleep_seconds), request_timeout_seconds = sqlc.arg(request_timeout_seconds),
  session_ttl_seconds = sqlc.arg(session_ttl_seconds), enabled = sqlc.arg(enabled), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetKB :one
SELECT * FROM kb_repos WHERE id = ?;

-- name: GetKBBySlug :one
SELECT * FROM kb_repos WHERE space_id = ? AND slug = ?;

-- name: ListKBs :many
SELECT * FROM kb_repos WHERE space_id = ? ORDER BY slug;

-- name: ListEnabledKBs :many
SELECT * FROM kb_repos WHERE enabled = 1 ORDER BY space_id, slug;

-- name: InsertKBSession :exec
INSERT INTO kb_sessions (id, kb_id, space_id, requester_key, requester_role, requester_label, instance_id, branch, state, last_active_at, created_at, updated_at, depth)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'new', ?, ?, ?, ?);

-- name: GetKBSession :one
SELECT * FROM kb_sessions WHERE id = ?;

-- name: GetKBSessionByRequester :one
SELECT * FROM kb_sessions WHERE kb_id = ? AND requester_key = ?;

-- name: GetKBSessionByRun :one
SELECT * FROM kb_sessions WHERE run_id = ? AND run_id != '';

-- name: ListKBSessions :many
SELECT * FROM kb_sessions WHERE kb_id = ? ORDER BY last_active_at DESC;

-- name: CountActiveKBSessions :one
SELECT COUNT(*) FROM kb_sessions WHERE kb_id = ? AND state = 'active';

-- name: CountActiveKBSessionsInSpace :one
SELECT COUNT(*) FROM kb_sessions WHERE space_id = ? AND state = 'active';

-- name: SetKBSessionRun :exec
UPDATE kb_sessions SET run_id = ?, state = ?, harness_session = ?, last_active_at = ?, updated_at = ? WHERE id = ?;

-- name: SetKBSessionState :exec
UPDATE kb_sessions SET state = ?, last_error = ?, failures = failures + ?, updated_at = ? WHERE id = ?;

-- name: TouchKBSession :exec
UPDATE kb_sessions SET last_active_at = ?, answered = answered + ?, updated_at = ? WHERE id = ?;

-- name: InsertKBRequest :exec
INSERT INTO kb_requests (id, kb_id, space_id, session_id, requester_run, requester_instance, requester_role, kind, question, task_id, state,
  deadline_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?);

-- name: GetKBRequest :one
SELECT * FROM kb_requests WHERE id = ?;

-- name: ListKBRequests :many
SELECT * FROM kb_requests WHERE kb_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: ListSessionRequests :many
SELECT * FROM kb_requests WHERE session_id = ? AND state IN ('queued', 'dispatched') ORDER BY created_at;

-- name: ListOpenKBRequests :many
SELECT * FROM kb_requests WHERE state IN ('queued', 'dispatched') ORDER BY created_at;

-- name: DispatchKBRequest :execrows
UPDATE kb_requests SET state = 'dispatched', message_id = ?, attempts = attempts + 1, dispatched_at = ?, updated_at = ?
WHERE id = ? AND state = 'queued';

-- name: AnswerKBRequest :execrows
UPDATE kb_requests SET state = sqlc.arg(state), answer = sqlc.arg(answer), basis = sqlc.arg(basis), evidence_json = sqlc.arg(evidence_json),
  answered_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'dispatched';

-- name: EndKBRequest :execrows
UPDATE kb_requests SET state = sqlc.arg(state), error = sqlc.arg(error), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('queued', 'dispatched');

-- name: RequeueKBRequest :execrows
UPDATE kb_requests SET state = 'queued', message_id = '', updated_at = ? WHERE id = ? AND state = 'dispatched';

-- name: CountKBRequests :one
SELECT
  COALESCE(SUM(CASE WHEN state IN ('queued', 'dispatched') THEN 1 ELSE 0 END), 0) AS open,
  COALESCE(SUM(CASE WHEN state = 'answered' THEN 1 ELSE 0 END), 0) AS answered,
  COALESCE(SUM(CASE WHEN state IN ('failed', 'timed_out') THEN 1 ELSE 0 END), 0) AS failed
FROM kb_requests WHERE kb_id = ?;

-- name: InsertKBProposal :exec
INSERT INTO kb_proposals (id, kb_id, space_id, session_id, request_id, title, summary, branch, commit_id, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'proposed', ?, ?);

-- name: GetKBProposal :one
SELECT * FROM kb_proposals WHERE id = ?;

-- name: ListKBProposals :many
SELECT * FROM kb_proposals WHERE kb_id = ? ORDER BY created_at DESC LIMIT 100;

-- name: NextSerializedProposal :one
SELECT * FROM kb_proposals WHERE kb_id = ? AND state = 'proposed' ORDER BY created_at LIMIT 1;

-- name: CountAcceptingProposals :one
SELECT COUNT(*) FROM kb_proposals WHERE kb_id = ? AND state = 'accepting';

-- name: SetKBProposalState :execrows
UPDATE kb_proposals SET state = sqlc.arg(state), merged_commit = sqlc.arg(merged_commit), detail = sqlc.arg(detail), command_id = sqlc.arg(command_id),
  decided_by = sqlc.arg(decided_by), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected);
