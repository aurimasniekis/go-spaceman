-- 0067_human_comment_notice: a person's plain comment on an open human action now reaches the agent that asked, as a
-- durable, routed notification of kind 'comment' (like 'clarification', but not a question: the action stays pending
-- and nothing needs an answer). human_notifications is rebuilt to allow the kind, because SQLite cannot alter a CHECK
-- constraint. The columns and indexes are those of 0031. The writer pool has one connection, so the foreign-key pragma
-- below applies to the statements that follow.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE human_notifications_new (
  id              TEXT PRIMARY KEY,
  action_id       TEXT NOT NULL REFERENCES human_actions (id) ON DELETE CASCADE,
  space_id        TEXT NOT NULL,
  target_run      TEXT NOT NULL DEFAULT '',
  target_session  TEXT NOT NULL DEFAULT '',
  target_role     TEXT NOT NULL DEFAULT '',
  kind            TEXT NOT NULL CHECK (kind IN ('resolved', 'cancelled', 'expired', 'clarification', 'comment')),
  state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'acknowledged')),
  created_at      INTEGER NOT NULL,
  delivered_at    INTEGER,
  notice_command  TEXT NOT NULL DEFAULT '',
  noticed_at      INTEGER,
  acknowledged_at INTEGER,
  target_kind     TEXT NOT NULL DEFAULT 'run' CHECK (target_kind IN ('run', 'instance', 'control', 'operators')),
  target_instance TEXT NOT NULL DEFAULT '',
  hold_until      INTEGER,
  wake            INTEGER NOT NULL DEFAULT 1,
  original_run    TEXT NOT NULL DEFAULT '',
  routed_from     TEXT NOT NULL DEFAULT '',
  route_reason    TEXT NOT NULL DEFAULT '',
  routed_at       INTEGER,
  route_count     INTEGER NOT NULL DEFAULT 0
) STRICT, WITHOUT ROWID;

INSERT INTO human_notifications_new (id, action_id, space_id, target_run, target_session, target_role, kind, state, created_at, delivered_at,
  notice_command, noticed_at, acknowledged_at, target_kind, target_instance, hold_until, wake, original_run, routed_from, route_reason, routed_at, route_count)
SELECT id, action_id, space_id, target_run, target_session, target_role, kind, state, created_at, delivered_at,
  notice_command, noticed_at, acknowledged_at, target_kind, target_instance, hold_until, wake, original_run, routed_from, route_reason, routed_at, route_count
FROM human_notifications;

DROP TABLE human_notifications;
ALTER TABLE human_notifications_new RENAME TO human_notifications;

CREATE INDEX ix_human_notifications_pending ON human_notifications (space_id, state, created_at);
CREATE INDEX ix_human_notifications_open ON human_notifications (state, target_kind, target_run);
CREATE INDEX ix_human_notifications_hold ON human_notifications (hold_until) WHERE hold_until IS NOT NULL AND state = 'pending';

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
