-- 0003_nodes_spaces_runs: nodes and their credentials, pinned spaces, execution leases, runs (desired vs observed)
-- and the node command log (SM-NODE-*, SM-SPACE-*, ADR-0006).

-- +goose Up
CREATE TABLE nodes (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  description       TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL CHECK (state IN ('pending', 'active', 'disabled', 'revoked')),
  labels_json       TEXT NOT NULL DEFAULT '{}',
  agent_version     TEXT NOT NULL DEFAULT '',
  protocol_minor    INTEGER NOT NULL DEFAULT 0,
  capabilities_json TEXT NOT NULL DEFAULT '{}',
  capacity          INTEGER NOT NULL DEFAULT 0,
  last_seen_at      INTEGER,
  last_connect_at   INTEGER,
  last_disconnect_at INTEGER,
  last_ip           TEXT NOT NULL DEFAULT '',
  version           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- One-time enrollment tokens (shown once, keyed MAC stored).
CREATE TABLE node_enrollments (
  mac        BLOB PRIMARY KEY,
  node_id    TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
  hint       TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL
) STRICT, WITHOUT ROWID;

-- Node credentials (spaceman_nd_…): keyed MAC stored; rotation overlaps; revocation closes live connections.
CREATE TABLE node_credentials (
  id           TEXT PRIMARY KEY,
  node_id      TEXT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
  mac          BLOB NOT NULL UNIQUE,
  hint         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,
  revoked_at   INTEGER,
  last_used_at INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_node_credentials_node ON node_credentials (node_id);

CREATE TABLE spaces (
  id                    TEXT PRIMARY KEY,
  slug                  TEXT NOT NULL UNIQUE,
  name                  TEXT NOT NULL,
  description           TEXT NOT NULL DEFAULT '',
  goals                 TEXT NOT NULL DEFAULT '',
  standing_instructions TEXT NOT NULL DEFAULT '',
  -- node_id is the effective pinned assignment; relocation sets desired_node_id first (SM-SPACE-006).
  node_id               TEXT NOT NULL REFERENCES nodes (id),
  desired_node_id       TEXT REFERENCES nodes (id),
  state                 TEXT NOT NULL CHECK (state IN ('active', 'paused', 'archived')),
  pause_mode            TEXT NOT NULL DEFAULT '' CHECK (pause_mode IN ('', 'hold', 'interrupt', 'emergency')),
  disconnect_policy     TEXT NOT NULL DEFAULT 'interrupt' CHECK (disconnect_policy IN ('interrupt', 'kill')),
  workdir               TEXT NOT NULL,
  version               INTEGER NOT NULL DEFAULT 1,
  created_at            INTEGER NOT NULL,
  created_by            TEXT NOT NULL,
  updated_at            INTEGER NOT NULL,
  archived_at           INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_spaces_node ON spaces (node_id);

-- One lease per space: the authority for a node to execute the space's runs (ADR-0006). generation increases on every
-- new grant (after expiry, relocation or restore) and fences everything older.
CREATE TABLE execution_leases (
  space_id   TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  node_id    TEXT NOT NULL REFERENCES nodes (id),
  generation INTEGER NOT NULL CHECK (generation > 0),
  state      TEXT NOT NULL CHECK (state IN ('active', 'revoked')),
  granted_at INTEGER NOT NULL,
  renewed_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE runs (
  id             TEXT PRIMARY KEY,
  space_id       TEXT NOT NULL REFERENCES spaces (id),
  node_id        TEXT NOT NULL REFERENCES nodes (id),
  generation     INTEGER NOT NULL,
  kind           TEXT NOT NULL CHECK (kind IN ('control', 'worker', 'kb', 'dreamer', 'diagnostic')),
  role           TEXT NOT NULL,
  harness        TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  instance_id    TEXT NOT NULL,
  session_id     TEXT NOT NULL DEFAULT '',
  desired        TEXT NOT NULL CHECK (desired IN ('running', 'stopped')),
  observed       TEXT NOT NULL CHECK (observed IN ('pending', 'starting', 'running', 'stopping', 'exited', 'failed', 'lost', 'unknown')),
  terminal_id    TEXT NOT NULL DEFAULT '',
  core_id        TEXT NOT NULL DEFAULT '',
  identity       TEXT NOT NULL DEFAULT '',
  pid            INTEGER NOT NULL DEFAULT 0,
  exit_code      INTEGER,
  end_reason     TEXT NOT NULL DEFAULT '',
  error          TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT '',
  status_kind    TEXT NOT NULL DEFAULT '',
  turn           INTEGER NOT NULL DEFAULT 0,
  adopted        INTEGER NOT NULL DEFAULT 0,
  launch_json    TEXT NOT NULL,
  manifest_json  TEXT NOT NULL DEFAULT '{}',
  version        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  created_by     TEXT NOT NULL,
  started_at     INTEGER,
  ended_at       INTEGER,
  observed_at    INTEGER,
  updated_at     INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_runs_space ON runs (space_id, created_at);
CREATE INDEX ix_runs_node_active ON runs (node_id, desired, observed);
-- At most one authoritative control run per space (SM-INV-006): a control run that is wanted or not yet ended.
CREATE UNIQUE INDEX ux_runs_one_control ON runs (space_id)
  WHERE kind = 'control' AND (desired = 'running' OR observed IN ('pending', 'starting', 'running', 'stopping', 'unknown'));

-- Commands to nodes. idempotency_key is unique per node; the node acknowledges duplicates without re-executing.
-- payload_json never contains secrets (proto.LaunchPayload.Redacted).
CREATE TABLE node_commands (
  id              TEXT PRIMARY KEY,
  node_id         TEXT NOT NULL REFERENCES nodes (id),
  space_id        TEXT NOT NULL,
  run_id          TEXT,
  generation      INTEGER NOT NULL,
  kind            TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  payload_json    TEXT NOT NULL DEFAULT '{}',
  state           TEXT NOT NULL CHECK (state IN ('pending', 'sent', 'accepted', 'duplicate', 'rejected', 'cancelled')),
  reason          TEXT NOT NULL DEFAULT '',
  attempts        INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  sent_at         INTEGER,
  acked_at        INTEGER,
  UNIQUE (node_id, idempotency_key)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_node_commands_pending ON node_commands (node_id, state, created_at);

-- Run credentials for the MCP bridge (ADR-0012): keyed MAC; valid while the run is active.
CREATE TABLE run_credentials (
  mac        BLOB PRIMARY KEY,
  run_id     TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_run_credentials_run ON run_credentials (run_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
