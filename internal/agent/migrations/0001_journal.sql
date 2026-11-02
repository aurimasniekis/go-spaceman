-- 0001_journal: the node agent's durable execution journal (SM-ARCH-002, SM-NODE-004/009).
-- Every command is recorded before any side effect; every run's intent is recorded before its process is started, so
-- a restart (of the agent, of herdr-core, or of central) reconciles instead of duplicating execution.

-- +goose Up
CREATE TABLE commands (
  id              TEXT PRIMARY KEY,
  kind            TEXT NOT NULL,
  space_id        TEXT NOT NULL,
  run_id          TEXT NOT NULL DEFAULT '',
  generation      INTEGER NOT NULL,
  idempotency_key TEXT NOT NULL UNIQUE,
  state           TEXT NOT NULL CHECK (state IN ('received', 'done', 'rejected')),
  reason          TEXT NOT NULL DEFAULT '',
  received_at     INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE runs (
  run_id        TEXT PRIMARY KEY,
  space_id      TEXT NOT NULL,
  generation    INTEGER NOT NULL,
  kind          TEXT NOT NULL,
  role          TEXT NOT NULL,
  harness       TEXT NOT NULL,
  instance_id   TEXT NOT NULL,
  terminal_name TEXT NOT NULL UNIQUE,
  terminal_id   TEXT NOT NULL DEFAULT '',
  core_id       TEXT NOT NULL DEFAULT '',
  identity      TEXT NOT NULL DEFAULT '',
  pid           INTEGER NOT NULL DEFAULT 0,
  desired       TEXT NOT NULL CHECK (desired IN ('running', 'stopped')),
  state         TEXT NOT NULL,
  exit_code     INTEGER,
  reason        TEXT NOT NULL DEFAULT '',
  error         TEXT NOT NULL DEFAULT '',
  session_id    TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL DEFAULT '',
  status_kind   TEXT NOT NULL DEFAULT '',
  turn          INTEGER NOT NULL DEFAULT 0,
  adopted       INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_runs_space ON runs (space_id);

CREATE TABLE leases (
  space_id    TEXT PRIMARY KEY,
  generation  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  workdir     TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE store_space_probe (
  id   INTEGER PRIMARY KEY CHECK (id = 1),
  data BLOB NOT NULL
) STRICT;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
