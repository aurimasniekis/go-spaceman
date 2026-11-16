-- 0060_pool_exhausted_notices: the people notice of an exhausted account pool, kept until its human action exists
-- (ADR-0090, plan T10). A park that finds the whole pool limited writes the row in its own transaction; the notice is
-- raised after the commit and the row deleted once the action exists, so a crash or a refused create (the space's open
-- action cap, an archived space) leaves the row for the next sweep. (space_id, dedup_key) is the human action's key.
-- The number may change at merge time, when another branch also takes it.

-- +goose Up
CREATE TABLE pool_exhausted_notices (
  space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  dedup_key  TEXT NOT NULL,
  wait_id    TEXT NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (space_id, dedup_key)
) STRICT, WITHOUT ROWID;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
