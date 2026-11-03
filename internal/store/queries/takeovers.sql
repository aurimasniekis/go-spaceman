-- Terminal takeovers (SM-UI-004).

-- name: InsertTakeover :exec
INSERT INTO run_takeovers (id, run_id, space_id, holder, holder_label, started_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetActiveTakeover :one
SELECT * FROM run_takeovers WHERE run_id = ? AND ended_at IS NULL;

-- name: GetTakeover :one
SELECT * FROM run_takeovers WHERE id = ?;

-- name: RenewTakeover :execrows
UPDATE run_takeovers SET expires_at = ? WHERE id = ? AND ended_at IS NULL;

-- name: EndTakeover :execrows
UPDATE run_takeovers SET ended_at = ?, ended_by = ?, end_reason = ? WHERE id = ? AND ended_at IS NULL;

-- name: ListExpiredTakeovers :many
SELECT * FROM run_takeovers WHERE ended_at IS NULL AND expires_at <= ? ORDER BY expires_at LIMIT 100;

-- name: NextTakeoverExpiry :one
SELECT CAST(COALESCE(MIN(expires_at), 0) AS INTEGER) AS expires_at FROM run_takeovers WHERE ended_at IS NULL;

-- name: ListNodeTakeovers :many
SELECT t.id, t.run_id, t.space_id, t.expires_at, r.generation
FROM run_takeovers t JOIN runs r ON r.id = t.run_id
WHERE t.ended_at IS NULL AND r.node_id = ?
ORDER BY t.started_at;
