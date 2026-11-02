-- name: GetIdempotency :one
SELECT * FROM idempotency_keys WHERE principal = ? AND operation = ? AND key = ? AND expires_at > ?;

-- name: InsertIdempotency :exec
INSERT INTO idempotency_keys (principal, operation, key, request_hash, status, body, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (principal, operation, key) DO NOTHING;

-- name: PurgeIdempotency :execrows
DELETE FROM idempotency_keys WHERE expires_at < ?;

-- name: InsertChange :one
INSERT INTO changes (at, space_id, resource_type, resource_id, action, version) VALUES (?, ?, ?, ?, ?, ?)
RETURNING seq;

-- name: ChangesAfter :many
SELECT * FROM changes WHERE seq > sqlc.arg(after) ORDER BY seq LIMIT sqlc.arg(lim);

-- name: ChangesAfterForSpaces :many
SELECT * FROM changes WHERE seq > sqlc.arg(after) AND (space_id IS NULL OR space_id IN (sqlc.slice(spaces))) ORDER BY seq LIMIT sqlc.arg(lim);

-- name: OldestChangeSeq :one
SELECT CAST(coalesce(min(seq), 0) AS INTEGER) AS oldest FROM changes;

-- name: LatestChangeSeq :one
SELECT CAST(coalesce(max(seq), 0) AS INTEGER) AS latest FROM changes;

-- name: PruneChanges :execrows
DELETE FROM changes WHERE at < ?;
