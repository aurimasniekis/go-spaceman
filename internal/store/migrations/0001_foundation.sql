-- 0001_foundation: settings, keyring, hash-chained audit log, durable jobs and the disk-full probe (ADR-0002).
-- Forward-only: rollback = restore the pre-migration snapshot.

-- +goose Up
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  updated_by TEXT
) STRICT, WITHOUT ROWID;

CREATE TABLE keyring_keys (
  id          TEXT PRIMARY KEY,
  purpose     TEXT NOT NULL CHECK (purpose IN ('field', 'credential', 'recording', 'manifest', 'token')),
  version     INTEGER NOT NULL CHECK (version > 0),
  wrapped     BLOB NOT NULL,
  wrap_kek_id TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('active', 'decrypt_only', 'retired')),
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL,
  UNIQUE (purpose, version)
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_keyring_active ON keyring_keys (purpose) WHERE state = 'active';

CREATE TABLE audit_events (
  id             TEXT PRIMARY KEY,
  seq            INTEGER NOT NULL UNIQUE CHECK (seq > 0),
  at             INTEGER NOT NULL,
  actor_type     TEXT NOT NULL CHECK (actor_type IN ('user', 'token', 'root', 'node', 'agent', 'system', 'policy', 'cli')),
  actor_id       TEXT,
  action         TEXT NOT NULL,
  target_type    TEXT,
  target_id      TEXT,
  outcome        TEXT NOT NULL CHECK (outcome IN ('success', 'failure', 'denied')),
  source_ip      TEXT,
  correlation_id TEXT,
  details_json   TEXT NOT NULL DEFAULT '{}',
  details_enc    BLOB,
  prev_hash      BLOB NOT NULL,
  hash           BLOB NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_audit_at ON audit_events (at);
CREATE INDEX ix_audit_target ON audit_events (target_type, target_id, at);
CREATE INDEX ix_audit_actor ON audit_events (actor_type, actor_id, at);
CREATE INDEX ix_audit_action ON audit_events (action, at);

CREATE TABLE store_space_probe (
  id   INTEGER PRIMARY KEY CHECK (id = 1),
  data BLOB NOT NULL
) STRICT;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
