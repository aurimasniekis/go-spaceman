-- 0023_backups: backups made by the running central (SM-STORE-005, ADR-0031). A backup's content lives in its target (a
-- local directory or S3-compatible object storage); this table keeps the run history and the last verification. Backups
-- made with the CLI exist only in the target until verified here.

-- +goose Up
CREATE TABLE backups (
  id            TEXT PRIMARY KEY,
  target        TEXT NOT NULL CHECK (target IN ('local', 's3')),
  trigger       TEXT NOT NULL CHECK (trigger IN ('manual', 'scheduled', 'cli')),
  requested_by  TEXT NOT NULL DEFAULT '',
  state         TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed', 'interrupted', 'deleted')),
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER,
  files         INTEGER NOT NULL DEFAULT 0,
  bytes         INTEGER NOT NULL DEFAULT 0,
  stored_bytes  INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  verify_state  TEXT NOT NULL DEFAULT '' CHECK (verify_state IN ('', 'running', 'ok', 'failed')),
  verified_at   INTEGER,
  verify_error  TEXT NOT NULL DEFAULT '',
  deleted_at    INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_backups_started ON backups (started_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
