-- Backups made by central (internal/backup, ADR-0031).

-- name: InsertBackup :exec
INSERT INTO backups (id, target, trigger, requested_by, state, started_at) VALUES (?, ?, ?, ?, 'running', ?);

-- name: FinishBackup :execrows
UPDATE backups SET state = sqlc.arg(state), finished_at = sqlc.arg(finished_at), files = sqlc.arg(files), bytes = sqlc.arg(bytes),
  stored_bytes = sqlc.arg(stored_bytes), error = sqlc.arg(error)
WHERE id = sqlc.arg(id) AND state = 'running';

-- name: GetBackup :one
SELECT * FROM backups WHERE id = ?;

-- name: ListBackups :many
SELECT * FROM backups WHERE state != 'deleted' ORDER BY started_at DESC LIMIT ?;

-- name: LastSucceededBackupAt :one
SELECT CAST(coalesce(max(started_at), 0) AS INTEGER) AS at FROM backups WHERE state IN ('succeeded', 'deleted');

-- name: LastFailedBackupAt :one
SELECT CAST(coalesce(max(started_at), 0) AS INTEGER) AS at FROM backups WHERE state IN ('failed', 'interrupted');

-- name: UpsertExternalBackup :exec
INSERT INTO backups (id, target, trigger, state, started_at, finished_at, files, bytes, stored_bytes)
VALUES (?, ?, 'cli', 'succeeded', ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING;

-- name: SetBackupVerify :execrows
UPDATE backups SET verify_state = sqlc.arg(verify_state), verified_at = sqlc.arg(verified_at), verify_error = sqlc.arg(verify_error)
WHERE id = sqlc.arg(id);

-- name: MarkBackupDeleted :execrows
UPDATE backups SET state = 'deleted', deleted_at = ? WHERE id = ?;

-- name: InterruptRunningBackups :execrows
UPDATE backups SET state = 'interrupted', finished_at = sqlc.arg(now), error = 'interrupted by a restart' WHERE state = 'running';

-- name: InterruptRunningVerifications :execrows
UPDATE backups SET verify_state = 'failed', verify_error = 'interrupted by a restart' WHERE verify_state = 'running';
