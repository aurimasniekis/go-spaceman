-- 0061_space_role_renames: renames of space roles (ADR-0093). A rename changes the role row and every live reference in
-- one transaction; this row is the durable intent behind the memory moves and resolves a former name to the current one.
--   from_name, to_name  the role's name before and after the rename
--   node_id             the space's node when the role was renamed (whose memory directory has to move)
--   node_state          pending: the node has not reported the move of its memory directory yet (launches of to_name
--                       and dream launches in the space are held); done: it has; not_needed: no node holds memory
--   node_message        the node's last message about the move (a deferral or an error), '' when none
--   node_done_at        when the node reported the move done
--   central_moved_at    when central moved its memory repository (NULL: not yet)

-- +goose Up
CREATE TABLE space_role_renames (
  id               TEXT PRIMARY KEY,
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  from_name        TEXT NOT NULL,
  to_name          TEXT NOT NULL,
  renamed_at       INTEGER NOT NULL,
  renamed_by       TEXT NOT NULL,
  node_id          TEXT NOT NULL DEFAULT '',
  node_state       TEXT NOT NULL CHECK (node_state IN ('pending', 'done', 'not_needed')),
  node_message     TEXT NOT NULL DEFAULT '',
  node_done_at     INTEGER,
  central_moved_at INTEGER,
  CHECK (from_name <> to_name)
) STRICT, WITHOUT ROWID;
CREATE INDEX ix_space_role_renames_from ON space_role_renames (space_id, from_name, renamed_at);
CREATE INDEX ix_space_role_renames_to ON space_role_renames (space_id, to_name);
CREATE INDEX ix_space_role_renames_pending ON space_role_renames (space_id) WHERE node_state = 'pending';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
