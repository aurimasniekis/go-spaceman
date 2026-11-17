-- 0062_role_rename_failed: a role rename's node move can fail (ADR-0093, protocol minor 14). The node answers a
-- memory.rename with ok, deferred or error; an error leaves the rename failed, and the role stays held until a later ok.
--   node_state  pending: the node has not moved the role's memory folder yet (the command waits for it, or it deferred);
--               failed: the node reported that it could not move it (node_message says why); done: it moved it;
--               not_needed: the space has no node. Launches of the role and dream launches in the space are held while
--               the state is pending or failed.
-- The table is rebuilt to widen the CHECK, and gains an index of the renames whose central repository has not moved yet
-- (memory.Service finishes them before it touches the space's memory, and at start).

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE space_role_renames_new (
  id               TEXT PRIMARY KEY,
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  from_name        TEXT NOT NULL,
  to_name          TEXT NOT NULL,
  renamed_at       INTEGER NOT NULL,
  renamed_by       TEXT NOT NULL,
  node_id          TEXT NOT NULL DEFAULT '',
  node_state       TEXT NOT NULL CHECK (node_state IN ('pending', 'failed', 'done', 'not_needed')),
  node_message     TEXT NOT NULL DEFAULT '',
  node_done_at     INTEGER,
  central_moved_at INTEGER,
  CHECK (from_name <> to_name)
) STRICT, WITHOUT ROWID;

INSERT INTO space_role_renames_new (id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state, node_message, node_done_at,
  central_moved_at)
SELECT id, space_id, from_name, to_name, renamed_at, renamed_by, node_id, node_state, node_message, node_done_at, central_moved_at
FROM space_role_renames;

DROP TABLE space_role_renames;
ALTER TABLE space_role_renames_new RENAME TO space_role_renames;

CREATE INDEX ix_space_role_renames_from ON space_role_renames (space_id, from_name, renamed_at);
CREATE INDEX ix_space_role_renames_to ON space_role_renames (space_id, to_name);
CREATE INDEX ix_space_role_renames_open ON space_role_renames (space_id) WHERE node_state IN ('pending', 'failed');
CREATE INDEX ix_space_role_renames_node ON space_role_renames (node_id) WHERE node_state IN ('pending', 'failed');
CREATE INDEX ix_space_role_renames_unmoved ON space_role_renames (space_id) WHERE central_moved_at IS NULL;

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
