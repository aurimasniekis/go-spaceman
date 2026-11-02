-- 0003_dreams: evidence positions of dreams (ADR-0027). A dream is offered transcript bytes after its configuration's
-- committed position; the positions it read are pending until its results are checkpointed, and only then committed.

-- +goose Up
CREATE TABLE dream_positions (
  config_id  TEXT NOT NULL,
  path       TEXT NOT NULL,
  offset     INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (config_id, path)
) STRICT, WITHOUT ROWID;

CREATE TABLE dream_pending (
  run_id    TEXT NOT NULL,
  config_id TEXT NOT NULL,
  path      TEXT NOT NULL,
  offset    INTEGER NOT NULL,
  PRIMARY KEY (run_id, path)
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE dream_pending;
DROP TABLE dream_positions;
