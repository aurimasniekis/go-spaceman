-- 0031_ui_gaps: clarification requests on human actions (SM-HUM-006), per-user notification preferences
-- (SM-HUI-006) and, when present below, terminal takeover state (SM-UI-004). ADR-0046.
--
-- human_notifications is rebuilt to allow kind 'clarification': a person asks the requester for more information
-- and the request reaches the requester through the same durable, routed notification as an outcome (SQLite cannot
-- alter a CHECK constraint). The columns keep their order. Comments get a kind: 'clarification' marks a comment that
-- asked the requester, 'reply' one the requester wrote back; both stay discussion and never count as a response.
-- The writer pool has one connection, so the foreign-key pragma below applies to the statements that follow.

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
  kind            TEXT NOT NULL CHECK (kind IN ('resolved', 'cancelled', 'expired', 'clarification')),
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

ALTER TABLE human_comments ADD COLUMN kind TEXT NOT NULL DEFAULT 'comment' CHECK (kind IN ('comment', 'clarification', 'reply'));

-- Per-user notification preferences (SM-HUI-006): which human-action changes the web UI announces to the user and
-- whether it also shows a desktop notification. SpaceMan sends no e-mail or chat notifications.
CREATE TABLE notification_prefs (
  user_id    TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  prefs_json TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

COMMIT;

PRAGMA foreign_keys = ON;

-- Statements of other parts of this migration follow (each section names its requirement).

-- Terminal takeover (SM-UI-004): an operator holding terminal:takeover types into a run's terminal while automated
-- input to it is refused. At most one active (not ended) takeover per run. It lasts while the holder renews it
-- (expires_at) and ends on release, on expiry (a closed page, a revoked session or token), or when the run ends. Rows
-- are kept as history; the keystrokes themselves are never stored.
BEGIN;

CREATE TABLE run_takeovers (
  id           TEXT PRIMARY KEY,
  run_id       TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
  space_id     TEXT NOT NULL,
  holder       TEXT NOT NULL,
  holder_label TEXT NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  ended_at     INTEGER,
  ended_by     TEXT NOT NULL DEFAULT '',
  end_reason   TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_run_takeovers_active ON run_takeovers (run_id) WHERE ended_at IS NULL;
CREATE INDEX ix_run_takeovers_expiry ON run_takeovers (expires_at) WHERE ended_at IS NULL;

COMMIT;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
