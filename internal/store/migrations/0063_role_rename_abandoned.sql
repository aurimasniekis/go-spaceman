-- 0063_role_rename_abandoned: an operator can close a stuck role rename (ADR-0093 addendum, T-H4). The node move is given up:
--   node_state  abandoned: an admin closed the rename while it was pending or failed. Central's memory stays under the new
--               name; the node's old-name folder is left on disk (orphaned), never deleted and never synced again.
--               node_done_at holds the close time and node_message the reason. Like done and not_needed it is not open:
--               holds lift, and another rename of either name or a clean relocation is no longer refused.
-- The table is rebuilt to widen the CHECK, the same shape as 0062. No data changes.

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
  node_state       TEXT NOT NULL CHECK (node_state IN ('pending', 'failed', 'done', 'not_needed', 'abandoned')),
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
