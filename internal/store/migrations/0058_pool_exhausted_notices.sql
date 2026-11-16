-- 0058_pool_exhausted_notices: the people notice of an exhausted account pool, kept until its human action exists
-- (ADR-0090, plan T10). A park that finds the whole pool limited writes the row in its own transaction; the notice is
-- raised after the commit and the row deleted once the action exists, so a crash or a refused create (the space's open
-- action cap) leaves the row for the next sweep. expires_at (UNIX microseconds) ends the window: the pool's reset, or the end of
-- the day when none is known; a row past it is dropped undelivered. (space_id, dedup_key) is the human action's key.

-- +goose Up
CREATE TABLE pool_exhausted_notices (
  space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  dedup_key  TEXT NOT NULL,
  wait_id    TEXT NOT NULL,
  title      TEXT NOT NULL,
  body       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  PRIMARY KEY (space_id, dedup_key)
) STRICT, WITHOUT ROWID;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
