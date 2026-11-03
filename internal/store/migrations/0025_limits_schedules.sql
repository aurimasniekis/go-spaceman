-- 0025_limits_schedules: one limits model, space schedules and explicit replays (SM-LIMIT-001…003, SM-EVT-015, ADR-0036).
--
-- space_limits holds a space's overrides of the installation limits (config `limits:`); a key that is absent inherits.
-- The effective value of every limit is computed from both and shown with its source. Lowering a limit never stops
-- running work unless the space's on_lower policy says stop (concurrency only).
--
-- run_tool_calls counts each run's agent tool calls for the per-run tool-call limit. budget_notices remembers which
-- budget warnings were sent, so a warning is sent once per window (budgets are usage-based and never a hard currency
-- limit: usage arrives late). task_deadline_notices remembers which running tasks passed their deadline (notified,
-- never stopped).
--
-- space_schedules create inbox items or tasks at cron or interval occurrences in an IANA time zone, with a missed-run
-- policy; schedule_occurrences records every occurrence once (its key is the deduplication key). One timer for the
-- earliest next occurrence drives them.
--
-- subscription_replays records each explicit replay of an event subscription (who, why, from where, and the boundary
-- before which a delivered event counts as replayed). Deliveries record their origin (live, redelivery, replay,
-- reprocess) and attempt, so a replay is never confused with a retry.
--
-- kb_sessions get a support depth (recursion guard), and inbox_items is rebuilt to allow the source kind 'schedule'
-- (SQLite cannot alter a CHECK constraint). The writer pool has one connection, so the foreign-key pragma below
-- applies to the statements that follow.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE space_limits (
  space_id    TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  values_json TEXT NOT NULL DEFAULT '{}',
  on_lower    TEXT NOT NULL DEFAULT 'drain' CHECK (on_lower IN ('drain', 'stop')),
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  updated_by  TEXT NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE run_tool_calls (
  run_id     TEXT PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  space_id   TEXT NOT NULL,
  calls      INTEGER NOT NULL DEFAULT 0,
  refused    INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE budget_notices (
  id       TEXT PRIMARY KEY,
  space_id TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  scope    TEXT NOT NULL CHECK (scope IN ('space', 'run')),
  run_id   TEXT NOT NULL DEFAULT '',
  budget   TEXT NOT NULL,
  level    TEXT NOT NULL CHECK (level IN ('warning', 'exceeded')),
  used     INTEGER NOT NULL,
  limit_value INTEGER NOT NULL,
  at       INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_budget_notices_space ON budget_notices (space_id, budget, level, at DESC);

CREATE TABLE task_deadline_notices (
  task_id    TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  started_at INTEGER NOT NULL,
  space_id   TEXT NOT NULL,
  at         INTEGER NOT NULL,
  PRIMARY KEY (task_id, started_at)
) STRICT, WITHOUT ROWID;

CREATE TABLE space_schedules (
  id               TEXT PRIMARY KEY,
  space_id         TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  description      TEXT NOT NULL DEFAULT '',
  enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  kind             TEXT NOT NULL CHECK (kind IN ('cron', 'interval')),
  cron             TEXT NOT NULL DEFAULT '',
  interval_seconds INTEGER NOT NULL DEFAULT 0,
  anchor_at        INTEGER NOT NULL,
  timezone         TEXT NOT NULL DEFAULT 'UTC',
  missed           TEXT NOT NULL DEFAULT 'skip' CHECK (missed IN ('skip', 'run_once', 'catch_up')),
  catch_up_max     INTEGER NOT NULL DEFAULT 3,
  skip_if_open     INTEGER NOT NULL DEFAULT 1 CHECK (skip_if_open IN (0, 1)),
  target           TEXT NOT NULL CHECK (target IN ('inbox', 'task')),
  title            TEXT NOT NULL,
  body             TEXT NOT NULL DEFAULT '',
  priority         TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
  assignee_role    TEXT NOT NULL DEFAULT '',
  next_at          INTEGER,
  last_at          INTEGER,
  last_outcome     TEXT NOT NULL DEFAULT '',
  last_target_id   TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  created_by       TEXT NOT NULL,
  updated_at       INTEGER NOT NULL,
  version          INTEGER NOT NULL DEFAULT 1,
  UNIQUE (space_id, name)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_space_schedules_next ON space_schedules (next_at) WHERE enabled = 1 AND next_at IS NOT NULL;

CREATE TABLE schedule_occurrences (
  schedule_id   TEXT NOT NULL REFERENCES space_schedules (id) ON DELETE CASCADE,
  occurrence_at INTEGER NOT NULL,
  space_id      TEXT NOT NULL,
  outcome       TEXT NOT NULL CHECK (outcome IN ('created', 'duplicate', 'skipped', 'missed', 'failed')),
  late          INTEGER NOT NULL DEFAULT 0 CHECK (late IN (0, 1)),
  target_kind   TEXT NOT NULL DEFAULT '',
  target_id     TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  at            INTEGER NOT NULL,
  PRIMARY KEY (schedule_id, occurrence_at)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_schedule_occurrences_recent ON schedule_occurrences (schedule_id, occurrence_at DESC);

CREATE TABLE subscription_replays (
  id              TEXT PRIMARY KEY,
  subscription_id TEXT NOT NULL REFERENCES event_subscriptions (id) ON DELETE CASCADE,
  space_id        TEXT NOT NULL,
  generation      INTEGER NOT NULL,
  policy          TEXT NOT NULL CHECK (policy IN ('all', 'time', 'seq')),
  start_time      INTEGER,
  start_seq       INTEGER,
  fresh_items     INTEGER NOT NULL DEFAULT 0 CHECK (fresh_items IN (0, 1)),
  reason          TEXT NOT NULL,
  requested_by    TEXT NOT NULL,
  requested_at    INTEGER NOT NULL,
  -- Set when the ingestion manager recreated the consumer: events stored in the stream before it are replayed ones.
  boundary_at     INTEGER,
  UNIQUE (subscription_id, generation)
) STRICT, WITHOUT ROWID;

ALTER TABLE event_deliveries ADD COLUMN origin TEXT NOT NULL DEFAULT 'live' CHECK (origin IN ('live', 'redelivery', 'replay', 'reprocess'));
ALTER TABLE event_deliveries ADD COLUMN attempt INTEGER NOT NULL DEFAULT 1;
ALTER TABLE event_deliveries ADD COLUMN replay_id TEXT NOT NULL DEFAULT '';
CREATE INDEX ix_event_deliveries_event ON event_deliveries (subscription_id, event_id);

ALTER TABLE kb_sessions ADD COLUMN depth INTEGER NOT NULL DEFAULT 1;

CREATE TABLE inbox_items_new (
  id             TEXT PRIMARY KEY,
  space_id       TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  source_kind    TEXT NOT NULL CHECK (source_kind IN ('operator', 'worker', 'human_response', 'dreamer', 'kb', 'cross_space', 'event', 'webhook', 'schedule')),
  source_id      TEXT NOT NULL DEFAULT '',
  source_label   TEXT NOT NULL DEFAULT '',
  title          TEXT NOT NULL,
  content_type   TEXT NOT NULL CHECK (content_type IN ('text/markdown', 'application/json')),
  body           TEXT NOT NULL DEFAULT '',
  payload_ref    TEXT NOT NULL DEFAULT '',
  correlation_id TEXT NOT NULL DEFAULT '',
  dedup_key      TEXT NOT NULL DEFAULT '',
  trusted_json   TEXT NOT NULL DEFAULT '{}',
  subscription_id TEXT NOT NULL DEFAULT '',
  urgency        TEXT NOT NULL DEFAULT 'normal' CHECK (urgency IN ('low', 'normal', 'high', 'urgent')),
  status         TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'in_progress', 'deferred', 'rejected', 'done')),
  status_reason  TEXT NOT NULL DEFAULT '',
  defer_until    INTEGER,
  linked_kind    TEXT NOT NULL DEFAULT '',
  linked_id      TEXT NOT NULL DEFAULT '',
  received_at    INTEGER NOT NULL,
  created_by     TEXT NOT NULL,
  updated_at     INTEGER NOT NULL,
  version        INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

INSERT INTO inbox_items_new (id, space_id, source_kind, source_id, source_label, title, content_type, body, payload_ref, correlation_id,
  dedup_key, trusted_json, subscription_id, urgency, status, status_reason, defer_until, linked_kind, linked_id, received_at, created_by,
  updated_at, version)
SELECT id, space_id, source_kind, source_id, source_label, title, content_type, body, payload_ref, correlation_id, dedup_key, trusted_json,
  subscription_id, urgency, status, status_reason, defer_until, linked_kind, linked_id, received_at, created_by, updated_at, version
FROM inbox_items;

DROP TABLE inbox_items;
ALTER TABLE inbox_items_new RENAME TO inbox_items;

CREATE INDEX ix_inbox_space ON inbox_items (space_id, received_at DESC, id DESC);
CREATE INDEX ix_inbox_space_status ON inbox_items (space_id, status, received_at DESC);
CREATE UNIQUE INDEX ux_inbox_dedup ON inbox_items (space_id, source_kind, dedup_key) WHERE dedup_key <> '';

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
