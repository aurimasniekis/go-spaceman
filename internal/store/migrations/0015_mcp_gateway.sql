-- 0015_mcp_gateway: external MCP servers reached through SpaceMan's gateway (SM-MCP-001/002, ADR-0023). Agents never see
-- a server's address or credentials: request headers are sealed (write-only), and a grant names the space and space role
-- whose runs may use the server's tools.

-- +goose Up
CREATE TABLE mcp_servers (
  id                TEXT PRIMARY KEY,
  slug              TEXT NOT NULL UNIQUE,
  name              TEXT NOT NULL,
  description       TEXT NOT NULL DEFAULT '',
  url               TEXT NOT NULL,
  headers_enc       BLOB,
  header_names_json TEXT NOT NULL DEFAULT '[]',
  headers_version   INTEGER NOT NULL DEFAULT 0,
  allow_private     INTEGER NOT NULL DEFAULT 0 CHECK (allow_private IN (0, 1)),
  tools_json        TEXT NOT NULL DEFAULT '[]',
  call_timeout_ms   INTEGER NOT NULL DEFAULT 20000,
  max_concurrency   INTEGER NOT NULL DEFAULT 4,
  max_result_bytes  INTEGER NOT NULL DEFAULT 262144,
  enabled           INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL,
  version           INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE mcp_grants (
  id         TEXT PRIMARY KEY,
  server_id  TEXT NOT NULL REFERENCES mcp_servers (id) ON DELETE CASCADE,
  space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  role       TEXT NOT NULL DEFAULT '*',
  tools_json TEXT NOT NULL DEFAULT '[]',
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  UNIQUE (server_id, space_id, role)
) STRICT, WITHOUT ROWID;

CREATE INDEX mcp_grants_space ON mcp_grants (space_id);

-- +goose Down
DROP TABLE mcp_grants;
DROP TABLE mcp_servers;
