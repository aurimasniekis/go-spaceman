-- 0011_messages: durable agent communication (SM-COMM-*). SpaceMan owns messages, requests, acknowledgement, replies and
-- correlation. State: stored (persisted, not yet seen) → available (a recipient can see it: notified or fetched) →
-- acknowledged (only through the agent's ack tool or an operator's explicit action, never inferred from terminal text)
-- → resolved (a request answered or closed).

-- +goose Up
CREATE TABLE messages (
  id               TEXT PRIMARY KEY,
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  from_kind        TEXT NOT NULL CHECK (from_kind IN ('run', 'operator', 'system')),
  from_id          TEXT NOT NULL DEFAULT '',
  from_label       TEXT NOT NULL DEFAULT '',
  to_kind          TEXT NOT NULL CHECK (to_kind IN ('run', 'role', 'control', 'operators')),
  to_id            TEXT NOT NULL DEFAULT '',
  kind             TEXT NOT NULL CHECK (kind IN ('message', 'request', 'reply', 'notification')),
  subject          TEXT NOT NULL,
  body             TEXT NOT NULL DEFAULT '',
  correlation_id   TEXT NOT NULL DEFAULT '',
  reply_to         TEXT NOT NULL DEFAULT '',
  dedup_key        TEXT NOT NULL DEFAULT '',
  state            TEXT NOT NULL DEFAULT 'stored' CHECK (state IN ('stored', 'available', 'acknowledged', 'resolved')),
  available_at     INTEGER,
  delivered_via    TEXT NOT NULL DEFAULT '',
  notice_command   TEXT NOT NULL DEFAULT '',
  noticed_at       INTEGER,
  acknowledged_at  INTEGER,
  acknowledged_by  TEXT NOT NULL DEFAULT '',
  resolved_at      INTEGER,
  resolution       TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  version          INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_messages_inbox ON messages (space_id, to_kind, to_id, state, created_at);
CREATE INDEX ix_messages_notice ON messages (notice_command) WHERE notice_command <> '';
CREATE UNIQUE INDEX ux_messages_dedup ON messages (space_id, from_kind, from_id, dedup_key) WHERE dedup_key <> '';

-- Human-action notifications are delivered by the same notifier.
ALTER TABLE human_notifications ADD COLUMN notice_command TEXT NOT NULL DEFAULT '';
ALTER TABLE human_notifications ADD COLUMN noticed_at INTEGER;
ALTER TABLE human_notifications ADD COLUMN acknowledged_at INTEGER;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
