-- name: InsertSpaceRepository :exec
INSERT INTO space_repositories (id, space_id, name, path, description, url, default_branch, role, clone_policy, known_hosts, git_key_id, created_at, created_by, updated_at, updated_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSpaceRepository :execrows
UPDATE space_repositories SET description = sqlc.arg(description), url = sqlc.arg(url), default_branch = sqlc.arg(default_branch), role = sqlc.arg(role),
  clone_policy = sqlc.arg(clone_policy), known_hosts = sqlc.arg(known_hosts), git_key_id = sqlc.arg(git_key_id), updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: GetSpaceRepository :one
SELECT * FROM space_repositories WHERE id = ? AND space_id = ?;

-- name: GetSpaceRepositoryByPath :one
SELECT * FROM space_repositories WHERE space_id = ? AND path = ?;

-- name: GetSpaceRepositoryByName :one
SELECT * FROM space_repositories WHERE space_id = ? AND name = ?;

-- name: ListSpaceRepositories :many
SELECT * FROM space_repositories WHERE space_id = ? ORDER BY name;

-- name: CountSpaceRepositories :one
SELECT count(*) FROM space_repositories WHERE space_id = ?;

-- name: DeleteSpaceRepository :execrows
DELETE FROM space_repositories WHERE id = ? AND space_id = ?;

-- name: ListKBsUsingRepository :many
SELECT slug FROM kb_repos WHERE space_id = ? AND repo = ? ORDER BY slug;

-- name: GetRepositoryReport :one
SELECT * FROM space_repository_reports WHERE space_id = ?;

-- name: SetRepositorySyncRequested :exec
INSERT INTO space_repository_reports (space_id, command_id, requested_at, requested_by) VALUES (?, ?, ?, ?)
ON CONFLICT (space_id) DO UPDATE SET command_id = excluded.command_id, requested_at = excluded.requested_at, requested_by = excluded.requested_by;

-- name: SetRepositoryReport :execrows
INSERT INTO space_repository_reports (space_id, node_id, report_json, reported_at, checked_at)
VALUES (sqlc.arg(space_id), sqlc.arg(node_id), sqlc.arg(report_json), sqlc.arg(reported_at), sqlc.arg(checked_at))
ON CONFLICT (space_id) DO UPDATE SET node_id = excluded.node_id, report_json = excluded.report_json, reported_at = excluded.reported_at,
  checked_at = excluded.checked_at
WHERE space_repository_reports.checked_at IS NULL OR space_repository_reports.checked_at <= excluded.checked_at
  OR space_repository_reports.node_id <> excluded.node_id;

-- name: SetRepositorySSHKey :execrows
UPDATE space_repositories SET ssh_key_enc = sqlc.arg(ssh_key_enc), ssh_public_key = sqlc.arg(ssh_public_key), ssh_key_created_at = sqlc.arg(created_at),
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE id = sqlc.arg(id) AND space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: GetRepositorySSHKey :one
SELECT id, space_id, url, ssh_key_enc, git_key_id FROM space_repositories WHERE id = ? AND space_id = ?;
