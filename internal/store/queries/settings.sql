-- name: GetSetting :one
SELECT key, value, updated_at, updated_by FROM settings WHERE key = ?;

-- name: UpsertSetting :exec
INSERT INTO settings (key, value, updated_at, updated_by) VALUES (?, ?, ?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at, updated_by = excluded.updated_by;

-- name: ListSettings :many
SELECT key, value, updated_at, updated_by FROM settings ORDER BY key;

