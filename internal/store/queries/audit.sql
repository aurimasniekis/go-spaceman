-- name: LastAuditEvent :one
SELECT seq, hash FROM audit_events ORDER BY seq DESC LIMIT 1;

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (id, seq, at, actor_type, actor_id, action, target_type, target_id, outcome, source_ip,
  correlation_id, details_json, details_enc, prev_hash, hash)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: AuditEventsFrom :many
SELECT id, seq, at, actor_type, actor_id, action, target_type, target_id, outcome, source_ip, correlation_id,
  details_json, details_enc, prev_hash, hash
FROM audit_events WHERE seq > ? ORDER BY seq LIMIT ?;

-- name: CountAuditEvents :one
SELECT count(*) FROM audit_events;

-- name: ListAuditPage :many
SELECT * FROM audit_events
WHERE (sqlc.narg(action) IS NULL OR action = sqlc.narg(action))
  AND (sqlc.narg(target_id) IS NULL OR target_id = sqlc.narg(target_id))
  AND (sqlc.narg(actor_id) IS NULL OR actor_id = sqlc.narg(actor_id))
  AND (sqlc.narg(before_seq) IS NULL OR seq < sqlc.narg(before_seq))
ORDER BY seq DESC LIMIT sqlc.arg(lim);
