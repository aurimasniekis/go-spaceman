-- Terminal dialogs of runs (ADR-0102).

-- name: InsertRunDialog :execrows
INSERT INTO run_dialogs (id, run_id, space_id, node_id, dialog, signal, outcome, answer, excerpt, harness_version, detected_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: GetRunDialog :one
SELECT * FROM run_dialogs WHERE id = ?;

-- name: ClearRunDialog :execrows
UPDATE run_dialogs SET cleared_at = ? WHERE id = ? AND run_id = ? AND outcome = 'blocked' AND cleared_at IS NULL;

-- name: ClearOpenRunDialogs :execrows
UPDATE run_dialogs SET cleared_at = ? WHERE run_id = ? AND outcome = 'blocked' AND cleared_at IS NULL;

-- name: ListRunDialogs :many
SELECT * FROM run_dialogs WHERE run_id = ? ORDER BY detected_at DESC, id DESC LIMIT ?;

-- name: ListOpenRunDialogs :many
SELECT * FROM run_dialogs WHERE run_id = ? AND outcome = 'blocked' AND cleared_at IS NULL ORDER BY detected_at, id;
