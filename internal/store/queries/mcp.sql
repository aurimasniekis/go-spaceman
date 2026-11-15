-- name: InsertMCPServer :exec
INSERT INTO mcp_servers (id, slug, name, description, url, headers_enc, header_names_json, headers_version, allow_private, tools_json,
  call_timeout_ms, max_concurrency, max_result_bytes, enabled, created_at, created_by, updated_at, kind, api_token_id, spaceman_json)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetMCPServer :one
SELECT * FROM mcp_servers WHERE id = ?;

-- name: GetMCPServerBySlug :one
SELECT * FROM mcp_servers WHERE slug = ?;

-- name: ListMCPServers :many
SELECT * FROM mcp_servers ORDER BY slug;

-- name: UpdateMCPServer :execrows
UPDATE mcp_servers SET name = sqlc.arg(name), description = sqlc.arg(description), url = sqlc.arg(url), allow_private = sqlc.arg(allow_private),
  tools_json = sqlc.arg(tools_json), call_timeout_ms = sqlc.arg(call_timeout_ms), max_concurrency = sqlc.arg(max_concurrency),
  max_result_bytes = sqlc.arg(max_result_bytes), enabled = sqlc.arg(enabled), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: SetMCPServerHeaders :execrows
UPDATE mcp_servers SET headers_enc = sqlc.arg(headers_enc), header_names_json = sqlc.arg(header_names_json), headers_version = headers_version + 1,
  updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: SetMCPServerSpaceman :execrows
UPDATE mcp_servers SET spaceman_json = sqlc.arg(spaceman_json), api_token_id = sqlc.arg(api_token_id), updated_at = sqlc.arg(now),
  version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) AND kind = 'spaceman';

-- name: DeleteMCPServer :execrows
DELETE FROM mcp_servers WHERE id = ?;

-- name: InsertMCPGrant :exec
INSERT INTO mcp_grants (id, server_id, space_id, role, tools_json, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetMCPGrant :one
SELECT * FROM mcp_grants WHERE id = ?;

-- name: GetMCPGrantByKey :one
SELECT * FROM mcp_grants WHERE server_id = ? AND space_id = ? AND role = ?;

-- name: DeleteMCPGrant :execrows
DELETE FROM mcp_grants WHERE id = ?;

-- name: ListMCPGrantsByServer :many
SELECT * FROM mcp_grants WHERE server_id = ? ORDER BY space_id, role;

-- name: ListMCPGrantsBySpace :many
SELECT * FROM mcp_grants WHERE space_id = ? ORDER BY server_id, role;

-- name: CountMCPGrants :one
SELECT COUNT(*) FROM mcp_grants WHERE server_id = ?;
