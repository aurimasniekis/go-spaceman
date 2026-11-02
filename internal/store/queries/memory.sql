-- name: GetMemoryCollection :one
SELECT * FROM memory_collections WHERE space_id = ? AND role = ?;

-- name: GetMemoryCollectionByID :one
SELECT * FROM memory_collections WHERE id = ?;

-- name: InsertMemoryCollection :exec
INSERT INTO memory_collections (id, space_id, role, created_at, updated_at) VALUES (?, ?, ?, ?, ?);

-- name: ListMemoryCollections :many
SELECT * FROM memory_collections WHERE space_id = ? ORDER BY role;

-- name: SetMemoryHead :execrows
UPDATE memory_collections SET head = sqlc.arg(head), head_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND head = sqlc.arg(expected);

-- name: SetMemoryNodeStatus :exec
UPDATE memory_collections SET node_status_json = ?, status_at = ?, updated_at = ? WHERE id = ?;

-- name: InsertMemoryConflict :exec
INSERT INTO memory_conflicts (id, collection_id, source, ref, commit_id, detail, created_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListMemoryConflicts :many
SELECT * FROM memory_conflicts WHERE collection_id = ? ORDER BY created_at DESC LIMIT 50;

-- name: CountOpenMemoryConflicts :one
SELECT COUNT(*) FROM memory_conflicts WHERE collection_id = ? AND resolved_at IS NULL;

-- name: GetMemoryConflict :one
SELECT * FROM memory_conflicts WHERE id = ?;

-- name: ResolveMemoryConflict :execrows
UPDATE memory_conflicts SET resolved_at = ?, resolved_by = ? WHERE id = ? AND resolved_at IS NULL;
