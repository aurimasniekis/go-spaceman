-- name: InsertGitKey :exec
INSERT INTO git_ssh_keys (id, name, description, public_key, private_enc, all_spaces, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetGitKey :one
SELECT * FROM git_ssh_keys WHERE id = ?;

-- name: GetGitKeyByName :one
SELECT * FROM git_ssh_keys WHERE name = ?;

-- name: ListGitKeys :many
SELECT * FROM git_ssh_keys ORDER BY name;

-- name: UpdateGitKey :execrows
UPDATE git_ssh_keys SET name = sqlc.arg(name), description = sqlc.arg(description), all_spaces = sqlc.arg(all_spaces),
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: RotateGitKey :execrows
UPDATE git_ssh_keys SET public_key = sqlc.arg(public_key), private_enc = sqlc.arg(private_enc), rotated_at = sqlc.arg(now),
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: DeleteGitKey :execrows
DELETE FROM git_ssh_keys WHERE id = ?;

-- name: ListGitKeySpaces :many
SELECT space_id FROM git_ssh_key_spaces WHERE key_id = ? ORDER BY space_id;

-- name: DeleteGitKeySpaces :exec
DELETE FROM git_ssh_key_spaces WHERE key_id = ?;

-- name: InsertGitKeySpace :exec
INSERT INTO git_ssh_key_spaces (key_id, space_id) VALUES (?, ?);

-- name: GitKeyAllowed :one
SELECT count(*) FROM git_ssh_keys k
WHERE k.id = sqlc.arg(id) AND (k.all_spaces = 1 OR EXISTS (SELECT 1 FROM git_ssh_key_spaces s WHERE s.key_id = k.id AND s.space_id = sqlc.arg(space_id)));

-- name: ListRepositoriesUsingGitKey :many
SELECT space_id, id, name FROM space_repositories WHERE git_key_id = ? ORDER BY space_id, name;

-- name: SetRepositoryGitKey :execrows
UPDATE space_repositories SET git_key_id = sqlc.arg(git_key_id), updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);
