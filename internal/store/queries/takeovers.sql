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

-- Terminal sizes (ADR-0075).

-- name: SetRunTerminalSize :execrows
UPDATE runs SET terminal_cols = sqlc.narg(term_cols), terminal_rows = sqlc.narg(term_rows), version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ListNodeTerminalSizes :many
SELECT id, space_id, generation, CAST(terminal_cols AS INTEGER) AS term_cols, CAST(terminal_rows AS INTEGER) AS term_rows
FROM runs
WHERE node_id = ? AND terminal_cols IS NOT NULL AND desired = 'running' AND observed IN ('pending', 'starting', 'running', 'unknown')
ORDER BY created_at;
