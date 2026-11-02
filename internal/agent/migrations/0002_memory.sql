-- 0002_memory: what the node knows about syncing each role-memory collection to central (ADR-0025): the head central
-- acknowledged, retries, the last error and conflicts preserved locally.

-- +goose Up
CREATE TABLE memory_sync (
  space_id           TEXT NOT NULL,
  role               TEXT NOT NULL,
  acked              TEXT NOT NULL DEFAULT '',
  last_ack_at        INTEGER,
  last_checkpoint_at INTEGER,
  retries            INTEGER NOT NULL DEFAULT 0,
  conflicts          INTEGER NOT NULL DEFAULT 0,
  last_error         TEXT NOT NULL DEFAULT '',
  updated_at         INTEGER NOT NULL,
  PRIMARY KEY (space_id, role)
) STRICT, WITHOUT ROWID;

-- +goose Down
DROP TABLE memory_sync;
