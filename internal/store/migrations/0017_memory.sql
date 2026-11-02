-- 0017_memory: role memory collections (SM-MEM-*, ADR-0025). A collection is the Git history of one role in one space,
-- kept on the space's node and preserved centrally. Central stores only acknowledged history (a fast-forward of what it
-- had); anything else is kept under refs/conflicts and listed here. The node's own view (uncheckpointed changes, local
-- commits, pending sync, retries) is reported and stored as status.

-- +goose Up
CREATE TABLE memory_collections (
  id               TEXT PRIMARY KEY,
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  role             TEXT NOT NULL,
  head             TEXT NOT NULL DEFAULT '',
  head_at          INTEGER,
  node_status_json TEXT NOT NULL DEFAULT '{}',
  status_at        INTEGER,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  UNIQUE (space_id, role)
) STRICT, WITHOUT ROWID;

CREATE TABLE memory_conflicts (
  id            TEXT PRIMARY KEY,
  collection_id TEXT NOT NULL REFERENCES memory_collections (id) ON DELETE CASCADE,
  source        TEXT NOT NULL CHECK (source IN ('central', 'node')),
  ref           TEXT NOT NULL DEFAULT '',
  commit_id     TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  resolved_at   INTEGER,
  resolved_by   TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

CREATE INDEX memory_conflicts_collection ON memory_conflicts (collection_id, created_at);

-- +goose Down
DROP TABLE memory_conflicts;
DROP TABLE memory_collections;
