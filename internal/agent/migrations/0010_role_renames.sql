-- 0010_role_renames: the node's journal of role renames (ADR-0093, protocol minor 14). One row per memory.rename the node
-- received: the space, the old and new role names, how far the move of the role's memory folder got (state), and the
-- outcome the node reports to central (result, message) until central acknowledges it (acked). A pending rename is tried
-- again when a run of the space ends (deferred) and at every agent start (deferred or error).
--
-- state: pending (not moved yet), moving (the old folder may already be under the new name; finished at start), done
-- (moved; result ok), dropped (central answered that it does not know the rename: never moved).

-- +goose Up
CREATE TABLE role_renames (
  rename_id   TEXT PRIMARY KEY,
  space_id    TEXT NOT NULL,
  from_role   TEXT NOT NULL,
  to_role     TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('pending', 'moving', 'done', 'dropped')),
  result      TEXT NOT NULL DEFAULT '',
  message     TEXT NOT NULL DEFAULT '',
  acked       INTEGER NOT NULL DEFAULT 0,
  reported_at INTEGER,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX role_renames_space ON role_renames (space_id, state);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
