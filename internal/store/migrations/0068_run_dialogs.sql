-- 0068_run_dialogs: interactive dialogs the node recognised on a run's terminal (ADR-0102). One row per occurrence,
-- keyed by the node's occurrence ID, so a repeated report records nothing twice. answered rows are the run's event log
-- of SpaceMan's own answers; blocked rows are dialogs it did not answer (the run showed blocked_on_dialog until
-- cleared_at). The excerpt is terminal output with secrets masked: shown in the run's notice, never published.

-- +goose Up
CREATE TABLE run_dialogs (
  id              TEXT PRIMARY KEY,
  run_id          TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  space_id        TEXT NOT NULL,
  node_id         TEXT NOT NULL,
  dialog          TEXT NOT NULL,
  signal          TEXT NOT NULL,
  outcome         TEXT NOT NULL CHECK (outcome IN ('answered', 'blocked')),
  answer          TEXT NOT NULL DEFAULT '',
  excerpt         TEXT NOT NULL DEFAULT '',
  harness_version TEXT NOT NULL DEFAULT '',
  detected_at     INTEGER NOT NULL,
  cleared_at      INTEGER,
  created_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_run_dialogs_run ON run_dialogs (run_id, detected_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
