-- name: InsertNode :exec
INSERT INTO nodes (id, name, description, state, labels_json, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: GetNodeByName :one
SELECT * FROM nodes WHERE name = ?;

-- name: ListNodes :many
SELECT * FROM nodes ORDER BY name;

-- name: UpdateNodeMeta :execrows
UPDATE nodes SET name = ?, description = ?, labels_json = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?;

-- name: SetNodeState :execrows
UPDATE nodes SET state = ?, version = version + 1, updated_at = ? WHERE id = ?;

-- name: RecordNodeConnect :exec
UPDATE nodes SET state = CASE WHEN state = 'pending' THEN 'active' ELSE state END, agent_version = sqlc.arg(agent_version),
  protocol_minor = sqlc.arg(protocol_minor), capabilities_json = sqlc.arg(capabilities_json), capacity = sqlc.arg(capacity),
  last_seen_at = sqlc.arg(now), last_connect_at = sqlc.arg(now), last_ip = sqlc.arg(last_ip), updated_at = sqlc.arg(now),
  capabilities_updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: RecordNodeCapabilities :execrows
UPDATE nodes SET capabilities_json = sqlc.arg(capabilities_json), capabilities_updated_at = sqlc.arg(now), last_seen_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: RecordNodeSeen :exec
UPDATE nodes SET last_seen_at = ? WHERE id = ?;

-- name: RecordNodeDisconnect :exec
UPDATE nodes SET last_disconnect_at = ?, updated_at = ? WHERE id = ?;

-- name: InsertNodeEnrollment :exec
INSERT INTO node_enrollments (mac, node_id, hint, expires_at, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetNodeEnrollment :one
SELECT * FROM node_enrollments WHERE mac = ?;

-- name: UseNodeEnrollment :execrows
UPDATE node_enrollments SET used_at = sqlc.arg(now) WHERE mac = sqlc.arg(mac) AND used_at IS NULL AND expires_at > sqlc.arg(now);

-- name: RevokeNodeEnrollments :execrows
UPDATE node_enrollments SET used_at = ? WHERE node_id = ? AND used_at IS NULL;

-- name: InsertNodeCredential :exec
INSERT INTO node_credentials (id, node_id, mac, hint, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: GetNodeCredentialByMAC :one
SELECT * FROM node_credentials WHERE mac = ?;

-- name: ListNodeCredentials :many
SELECT * FROM node_credentials WHERE node_id = ? ORDER BY created_at DESC;

-- name: RevokeNodeCredentials :execrows
UPDATE node_credentials SET revoked_at = sqlc.arg(now) WHERE node_id = sqlc.arg(node_id) AND revoked_at IS NULL AND id != sqlc.arg(except_id);

-- name: ExpireNodeCredential :exec
UPDATE node_credentials SET expires_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: TouchNodeCredential :exec
UPDATE node_credentials SET last_used_at = ? WHERE id = ?;

-- name: InsertSpace :exec
INSERT INTO spaces (id, slug, name, description, goals, standing_instructions, node_id, state, disconnect_policy, workdir, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?);

-- name: GetSpace :one
SELECT * FROM spaces WHERE id = ?;

-- name: GetSpaceBySlug :one
SELECT * FROM spaces WHERE slug = ?;

-- name: ListSpaces :many
SELECT * FROM spaces ORDER BY name, id;

-- name: ListSpacesByNode :many
SELECT * FROM spaces WHERE node_id = ? ORDER BY id;

-- name: UpdateSpaceProfile :execrows
UPDATE spaces SET name = ?, description = ?, goals = ?, standing_instructions = ?, disconnect_policy = ?, version = version + 1, updated_at = ?
WHERE id = ? AND version = ?;

-- name: SetSpaceState :execrows
UPDATE spaces SET state = sqlc.arg(state), pause_mode = sqlc.arg(pause_mode),
  archived_at = CASE WHEN sqlc.arg(state) = 'archived' THEN sqlc.arg(now) ELSE NULL END,
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: DeleteSpace :execrows
DELETE FROM spaces WHERE id = ? AND state = 'archived';

-- name: GetLease :one
SELECT * FROM execution_leases WHERE space_id = ?;

-- name: ListLeasesByNode :many
SELECT * FROM execution_leases WHERE node_id = ? ORDER BY space_id;

-- name: UpsertLease :exec
INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at, grace_us) VALUES (?, ?, ?, 'active', ?, ?, ?, ?)
ON CONFLICT (space_id) DO UPDATE SET node_id = excluded.node_id, generation = excluded.generation, state = 'active',
  granted_at = excluded.granted_at, renewed_at = excluded.renewed_at, expires_at = excluded.expires_at, grace_us = excluded.grace_us;

-- name: RenewLease :execrows
UPDATE execution_leases SET renewed_at = sqlc.arg(now), expires_at = sqlc.arg(expires_at)
WHERE space_id = sqlc.arg(space_id) AND node_id = sqlc.arg(node_id) AND generation = sqlc.arg(generation) AND state = 'active' AND expires_at > sqlc.arg(now);

-- name: RevokeLease :execrows
UPDATE execution_leases SET state = 'revoked', expires_at = sqlc.arg(now) WHERE space_id = sqlc.arg(space_id) AND state = 'active';

-- name: InsertRun :exec
INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, desired, observed, launch_json, manifest_json,
  created_at, created_by, updated_at, terminal_cols, terminal_rows)
VALUES (sqlc.arg(id), sqlc.arg(space_id), sqlc.arg(node_id), sqlc.arg(generation), sqlc.arg(kind), sqlc.arg(role), sqlc.arg(harness),
  sqlc.arg(instance_id), sqlc.arg(session_id), 'running', 'pending', sqlc.arg(launch_json), sqlc.arg(manifest_json), sqlc.arg(created_at),
  sqlc.arg(created_by), sqlc.arg(updated_at), sqlc.narg(terminal_cols), sqlc.narg(terminal_rows));

-- name: GetRun :one
SELECT * FROM runs WHERE id = ?;

-- name: ListRunsBySpace :many
SELECT * FROM runs WHERE space_id = ? ORDER BY created_at DESC, id DESC LIMIT ?;

-- The active runs of a node or a space are its live ones (fleet.Live: observed not exited, failed or lost). desired is
-- not consulted: a run that ended on its own keeps desired = 'running', and counting it blocked archive and relocation.
-- name: ListActiveRunsByNode :many
SELECT * FROM runs WHERE node_id = ? AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown') ORDER BY created_at;

-- name: ListActiveRunsBySpace :many
SELECT * FROM runs WHERE space_id = ? AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown') ORDER BY created_at;

-- name: SetRunDesired :execrows
UPDATE runs SET desired = ?, version = version + 1, updated_at = ? WHERE id = ?;

-- name: UpdateRunObserved :execrows
UPDATE runs SET observed = sqlc.arg(observed), terminal_id = sqlc.arg(terminal_id), core_id = sqlc.arg(core_id), identity = sqlc.arg(identity),
  pid = sqlc.arg(pid), exit_code = sqlc.arg(exit_code), end_reason = sqlc.arg(end_reason), error = sqlc.arg(error), status = sqlc.arg(status),
  status_kind = sqlc.arg(status_kind), turn = sqlc.arg(turn), adopted = sqlc.arg(adopted),
  session_id = CASE WHEN sqlc.arg(session_id) = '' THEN session_id ELSE sqlc.arg(session_id) END,
  started_at = CASE WHEN started_at IS NULL AND sqlc.arg(mark_started) = 1 THEN sqlc.arg(now) ELSE started_at END,
  ended_at = CASE WHEN sqlc.arg(mark_ended) = 1 THEN coalesce(ended_at, sqlc.arg(now)) ELSE NULL END,
  observed_at = sqlc.arg(now), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND generation = sqlc.arg(generation);

-- name: SetRunHarness :execrows
UPDATE runs SET harness_version = sqlc.arg(harness_version), harness_requested = sqlc.arg(harness_requested),
  harness_fallback = sqlc.arg(harness_fallback), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND generation = sqlc.arg(generation)
  AND (harness_version != sqlc.arg(harness_version) OR harness_requested != sqlc.arg(harness_requested) OR harness_fallback != sqlc.arg(harness_fallback));

-- name: MarkNodeRunsUnknown :execrows
UPDATE runs SET observed = 'unknown', version = version + 1, updated_at = sqlc.arg(now)
WHERE node_id = sqlc.arg(node_id) AND observed IN ('pending', 'starting', 'running', 'stopping');

-- name: InsertNodeCommand :exec
INSERT INTO node_commands (id, node_id, space_id, run_id, generation, kind, idempotency_key, payload_json, state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)
ON CONFLICT (node_id, idempotency_key) DO NOTHING;

-- name: GetNodeCommand :one
SELECT * FROM node_commands WHERE id = ?;

-- name: ListUndeliveredCommands :many
SELECT * FROM node_commands WHERE node_id = ? AND state IN ('pending', 'sent') ORDER BY created_at, id;

-- name: MarkCommandSent :exec
UPDATE node_commands SET state = CASE WHEN state = 'pending' THEN 'sent' ELSE state END, attempts = attempts + 1, sent_at = ? WHERE id = ?;

-- name: AckCommand :execrows
UPDATE node_commands SET state = ?, reason = ?, acked_at = ? WHERE id = ? AND node_id = ? AND state IN ('pending', 'sent');

-- name: CancelRunCommands :execrows
UPDATE node_commands SET state = 'cancelled', reason = ? WHERE run_id = ? AND state IN ('pending', 'sent') AND kind = 'run.launch';

-- name: ListCommandsByRun :many
SELECT * FROM node_commands WHERE run_id = ? ORDER BY created_at;

-- name: InsertRunCredential :exec
INSERT INTO run_credentials (mac, run_id, created_at, token_enc) VALUES (?, ?, ?, ?);

-- name: GetActiveRunToken :one
SELECT token_enc FROM run_credentials WHERE run_id = ? AND revoked_at IS NULL AND token_enc IS NOT NULL ORDER BY created_at DESC LIMIT 1;

-- name: GetRunCredential :one
SELECT c.mac, c.run_id, c.revoked_at, r.space_id, r.kind, r.role, r.instance_id, r.desired, r.observed, r.generation
FROM run_credentials c JOIN runs r ON r.id = c.run_id WHERE c.mac = ?;

-- name: RevokeRunCredentials :execrows
UPDATE run_credentials SET revoked_at = ? WHERE run_id = ? AND revoked_at IS NULL;

-- name: CountRunsCreatedBy :one
SELECT count(*) FROM runs WHERE space_id = ? AND kind = ? AND created_by = ? AND created_at >= ?;

-- name: GetLatestControlRun :one
SELECT * FROM runs WHERE space_id = ? AND kind = 'control' ORDER BY created_at DESC, id DESC LIMIT 1;

-- A control run waiting for its usage limit to reset is waiting, not idle: its idle stop is not armed (ADR-0079 decision 3).
-- name: ListIdleControlRuns :many
SELECT id FROM runs WHERE kind = 'control' AND desired = 'running' AND observed = 'running' AND status IN ('idle', 'done') AND limit_kind = '';

-- Runs the stale-run sweeper looks at (fleet/stale.go, ADR-0074): live runs past their launch (starting, running,
-- stopping or unknown) whose space is still pinned to the run's node (relocation owns the others), with the space's
-- lease while it is active (a revoked lease's stored expiry is the revoke time, not the expiry the node holds). A run is never a candidate while a launch command for it is unacknowledged (pending or sent: the dispatch loop
-- may still deliver it) or when no node has ever reported it (observed_at is NULL: its launch may still be in flight).
-- name: ListStaleRunCandidates :many
SELECT r.id, r.node_id, r.space_id, r.version, r.observed, CAST(coalesce(r.observed_at, 0) AS INTEGER) AS last_observed_at,
  CAST(coalesce(l.node_id, '') AS TEXT) AS lease_node_id, CAST(coalesce(l.expires_at, 0) AS INTEGER) AS lease_expires_at,
  CAST(coalesce(l.grace_us, 0) AS INTEGER) AS lease_grace_us
FROM nodes n
JOIN runs r ON r.node_id = n.id
JOIN spaces s ON s.id = r.space_id AND s.node_id = r.node_id
LEFT JOIN execution_leases l ON l.space_id = r.space_id AND l.state = 'active'
WHERE r.observed IN ('starting', 'running', 'stopping', 'unknown') AND r.observed_at IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM node_commands c WHERE c.run_id = r.id AND c.kind = 'run.launch' AND c.state IN ('pending', 'sent'))
ORDER BY r.id;

-- Every undelivered or unacknowledged command of a run, whatever its kind (the stale-run sweeper's end transaction:
-- nothing for a run it ended may reach a node).
-- name: CancelAllRunCommands :execrows
UPDATE node_commands SET state = 'cancelled', reason = ? WHERE run_id = ? AND state IN ('pending', 'sent');

-- Whether a run has an unacknowledged launch command (the stale-run sweeper re-checks it in the run's end transaction).
-- name: CountUnackedRunLaunches :one
SELECT count(*) FROM node_commands WHERE run_id = ? AND kind = 'run.launch' AND state IN ('pending', 'sent');

-- The runs of a node that are live when it connects, with their versions (the stale-run sweeper's connect snapshot).
-- name: ListLiveRunVersionsByNode :many
SELECT id, version FROM runs WHERE node_id = ? AND observed IN ('pending', 'starting', 'running', 'stopping', 'unknown') ORDER BY id;
