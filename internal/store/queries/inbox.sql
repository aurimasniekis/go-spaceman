-- name: InsertInboxItem :execrows
INSERT INTO inbox_items (id, space_id, source_kind, source_id, source_label, title, content_type, body, payload_ref, correlation_id,
  dedup_key, trusted_json, subscription_id, urgency, received_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: GetInboxItem :one
SELECT * FROM inbox_items WHERE id = ?;

-- name: GetInboxItemByDedup :one
SELECT * FROM inbox_items WHERE space_id = ? AND source_kind = ? AND dedup_key = ?;

-- name: ListInboxItems :many
SELECT * FROM inbox_items
WHERE space_id = sqlc.arg(space_id)
  AND (sqlc.arg(status) = '' OR status = sqlc.arg(status))
  AND (sqlc.arg(before_at) = 0 OR received_at < sqlc.arg(before_at) OR (received_at = sqlc.arg(before_at) AND id < sqlc.arg(before_id)))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(lim);

-- name: CountInboxByStatus :many
SELECT status, count(*) AS n FROM inbox_items WHERE space_id = ? GROUP BY status;

-- name: UpdateInboxStatus :execrows
UPDATE inbox_items SET status = sqlc.arg(status), status_reason = sqlc.arg(status_reason), defer_until = sqlc.arg(defer_until),
  linked_kind = sqlc.arg(linked_kind), linked_id = sqlc.arg(linked_id), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);
