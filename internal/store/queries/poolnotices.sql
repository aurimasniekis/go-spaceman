-- The people notice of an exhausted pool, kept until its human action exists (migration 0058). A second park in the
-- same window keeps the first row.
-- name: InsertPoolExhaustedNotice :exec
INSERT INTO pool_exhausted_notices (space_id, dedup_key, wait_id, title, body, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (space_id, dedup_key) DO NOTHING;

-- name: ListPoolExhaustedNotices :many
SELECT * FROM pool_exhausted_notices ORDER BY created_at, space_id, dedup_key LIMIT ?;

-- name: DeletePoolExhaustedNotice :exec
DELETE FROM pool_exhausted_notices WHERE space_id = ? AND dedup_key = ?;

-- A notice whose window is over is dropped undelivered: the reset has passed, or the day has ended.
-- name: DeleteExpiredPoolExhaustedNotices :execrows
DELETE FROM pool_exhausted_notices WHERE expires_at <= ?;
