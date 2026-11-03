-- name: InsertAttachment :exec
INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAttachment :one
SELECT * FROM attachments WHERE id = ?;

-- name: SpaceAttachmentBytes :one
SELECT CAST(coalesce(sum(size), 0) AS INTEGER) AS total FROM attachments WHERE space_id = ?;

-- name: InsertAttachmentRef :exec
INSERT INTO attachment_refs (attachment_id, owner_kind, owner_id, created_at) VALUES (?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: ListAttachmentsByOwner :many
SELECT a.* FROM attachments a JOIN attachment_refs r ON r.attachment_id = a.id
WHERE r.owner_kind = sqlc.arg(owner_kind) AND r.owner_id = sqlc.arg(owner_id) ORDER BY a.created_at, a.id;

-- name: DeleteAttachmentRefsByOwner :execrows
DELETE FROM attachment_refs WHERE owner_kind = ? AND owner_id = ?;

-- name: DeleteUnreferencedAttachments :execrows
DELETE FROM attachments WHERE attachments.created_at < sqlc.arg(before)
  AND NOT EXISTS (SELECT 1 FROM attachment_refs r WHERE r.attachment_id = attachments.id);

-- name: CountAttachmentsBySHA :one
SELECT count(*) FROM attachments WHERE sha256 = ?;

-- name: ListAttachmentsByOwners :many
SELECT r.owner_id, a.id, a.space_id, a.sha256, a.size, a.mime, a.name, a.uploaded_by, a.created_at
FROM attachments a JOIN attachment_refs r ON r.attachment_id = a.id
WHERE r.owner_kind = sqlc.arg(owner_kind) AND r.owner_id IN (sqlc.slice(owner_ids)) ORDER BY a.created_at, a.id;
