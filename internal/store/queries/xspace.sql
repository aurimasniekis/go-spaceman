-- Cross-space requests (ADR-0038).

-- name: GetXSpaceSettings :one
SELECT * FROM xspace_settings WHERE space_id = ?;

-- name: InsertXSpaceSettings :exec
INSERT INTO xspace_settings (space_id, profile, max_depth, max_outgoing_per_hour, version, updated_at, updated_by) VALUES (?, ?, ?, ?, 1, ?, ?);

-- name: UpdateXSpaceSettings :execrows
UPDATE xspace_settings SET profile = sqlc.arg(profile), max_depth = sqlc.arg(max_depth), max_outgoing_per_hour = sqlc.arg(max_outgoing_per_hour),
  version = version + 1, updated_at = sqlc.arg(now), updated_by = sqlc.arg(by)
WHERE space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: GetXSpacePeer :one
SELECT * FROM xspace_peers WHERE space_id = ? AND peer_space_id = ?;

-- name: ListXSpacePeers :many
SELECT * FROM xspace_peers WHERE space_id = ? ORDER BY peer_space_id;

-- name: ListAllXSpacePeers :many
SELECT * FROM xspace_peers ORDER BY space_id, peer_space_id;

-- name: ListXSpaceDiscoverable :many
SELECT * FROM xspace_peers WHERE peer_space_id = ? AND in_discover = 1 ORDER BY space_id;

-- name: InsertXSpacePeer :exec
INSERT INTO xspace_peers (space_id, peer_space_id, in_discover, in_questions, in_delegation, in_knowledge, in_kbs_json, disclosure,
  max_per_hour, max_pending, out_questions, out_delegation, out_knowledge, version, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?);

-- name: UpdateXSpacePeer :execrows
UPDATE xspace_peers SET in_discover = sqlc.arg(in_discover), in_questions = sqlc.arg(in_questions), in_delegation = sqlc.arg(in_delegation),
  in_knowledge = sqlc.arg(in_knowledge), in_kbs_json = sqlc.arg(in_kbs_json), disclosure = sqlc.arg(disclosure),
  max_per_hour = sqlc.arg(max_per_hour), max_pending = sqlc.arg(max_pending), out_questions = sqlc.arg(out_questions),
  out_delegation = sqlc.arg(out_delegation), out_knowledge = sqlc.arg(out_knowledge), version = version + 1,
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(by)
WHERE space_id = sqlc.arg(space_id) AND peer_space_id = sqlc.arg(peer_space_id) AND version = sqlc.arg(version);

-- name: DeleteXSpacePeer :execrows
DELETE FROM xspace_peers WHERE space_id = ? AND peer_space_id = ?;

-- name: InsertXSpaceRequest :exec
INSERT INTO xspace_requests (id, source_space_id, dest_space_id, kind, title, body, kb_slug, kb_kind, requester_run, requester_role,
  correlation_id, caused_by, chain_json, depth, state, deadline_at, inbox_item_id, kb_request_id, disclosure, decided_by, decided_at, dedup_key,
  created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetXSpaceRequest :one
SELECT * FROM xspace_requests WHERE id = ?;

-- name: GetXSpaceRequestByDedup :one
SELECT * FROM xspace_requests WHERE source_space_id = ? AND requester_run = ? AND dedup_key = ?;

-- name: GetXSpaceRequestByKB :one
SELECT * FROM xspace_requests WHERE kb_request_id = ?;

-- name: ListXSpaceOutgoing :many
SELECT * FROM xspace_requests WHERE source_space_id = sqlc.arg(space_id)
  AND (CAST(sqlc.arg(open_only) AS INTEGER) = 0 OR state IN ('pending', 'accepted'))
ORDER BY created_at DESC LIMIT sqlc.arg(lim);

-- name: ListXSpaceIncoming :many
SELECT * FROM xspace_requests WHERE dest_space_id = sqlc.arg(space_id)
  AND (CAST(sqlc.arg(open_only) AS INTEGER) = 0 OR state IN ('pending', 'accepted'))
ORDER BY created_at DESC LIMIT sqlc.arg(lim);

-- name: ListXSpaceRequests :many
SELECT * FROM xspace_requests WHERE (CAST(sqlc.arg(open_only) AS INTEGER) = 0 OR state IN ('pending', 'accepted'))
ORDER BY created_at DESC LIMIT sqlc.arg(lim);

-- name: ListOpenXSpaceIncoming :many
SELECT * FROM xspace_requests WHERE dest_space_id = ? AND state IN ('pending', 'accepted') ORDER BY created_at;

-- name: CountXSpacePairSince :one
SELECT COUNT(*) FROM xspace_requests WHERE source_space_id = ? AND dest_space_id = ? AND created_at >= ?;

-- name: CountXSpacePairOpen :one
SELECT COUNT(*) FROM xspace_requests WHERE source_space_id = ? AND dest_space_id = ? AND state IN ('pending', 'accepted');

-- name: CountXSpaceOutgoingSince :one
SELECT COUNT(*) FROM xspace_requests WHERE source_space_id = ? AND created_at >= ?;

-- name: NextXSpaceDeadline :one
SELECT CAST(COALESCE(MIN(deadline_at), 0) AS INTEGER) FROM xspace_requests WHERE state IN ('pending', 'accepted');

-- name: DueXSpaceRequests :many
SELECT * FROM xspace_requests WHERE state IN ('pending', 'accepted') AND deadline_at <= ? ORDER BY deadline_at LIMIT ?;

-- name: AcceptXSpaceRequest :execrows
UPDATE xspace_requests SET state = 'accepted', decided_by = sqlc.arg(decided_by), decided_at = sqlc.arg(now), local_task_id = sqlc.arg(local_task_id),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND state = 'pending';

-- name: CloseXSpaceRequest :execrows
UPDATE xspace_requests SET state = sqlc.arg(state), disclosure = sqlc.arg(disclosure), reply = sqlc.arg(reply), reply_truncated = sqlc.arg(reply_truncated),
  reply_basis = sqlc.arg(reply_basis), reply_evidence_json = sqlc.arg(reply_evidence_json), reason = sqlc.arg(reason),
  decided_by = CASE WHEN decided_by = '' THEN sqlc.arg(decided_by) ELSE decided_by END, decided_at = COALESCE(decided_at, sqlc.narg(decided_at)),
  closed_at = sqlc.narg(now_n), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND state IN ('pending', 'accepted');

-- name: SetKBRequestXSpace :exec
UPDATE kb_requests SET xspace_request_id = ? WHERE id = ?;
