-- 0006_events: event connections, the template catalog, space subscriptions, delivery history and quarantine
-- (SM-EVT-*, ADR-0011). Connections, templates and subscriptions are separate entities (SM-EVT-003).

-- +goose Up
CREATE TABLE event_connections (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  kind        TEXT NOT NULL CHECK (kind IN ('pulse')),
  url         TEXT NOT NULL,
  -- .creds content sealed with the keyring (purpose credential); never returned by the API.
  creds_enc   BLOB NOT NULL,
  client_id   TEXT NOT NULL,
  ca_pem      TEXT NOT NULL DEFAULT '',
  stream      TEXT NOT NULL DEFAULT 'EVENTS',
  enabled     INTEGER NOT NULL DEFAULT 1,
  version     INTEGER NOT NULL DEFAULT 1,
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE event_templates (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  builtin     INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- Published versions are immutable (SM-EVT-004); drafts can be replaced until they are published.
CREATE TABLE event_template_versions (
  id              TEXT PRIMARY KEY,
  template_id     TEXT NOT NULL REFERENCES event_templates (id) ON DELETE CASCADE,
  version         INTEGER NOT NULL,
  status          TEXT NOT NULL CHECK (status IN ('draft', 'published')),
  definition_json TEXT NOT NULL,
  notes           TEXT NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  created_by      TEXT NOT NULL,
  published_at    INTEGER,
  UNIQUE (template_id, version)
) STRICT, WITHOUT ROWID;

CREATE TABLE event_subscriptions (
  id                  TEXT PRIMARY KEY,
  space_id            TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  name                TEXT NOT NULL,
  connection_id       TEXT NOT NULL REFERENCES event_connections (id),
  -- Exactly one of a template version (pinned, or followed to the latest published) or a custom definition.
  template_id         TEXT REFERENCES event_templates (id),
  template_version_id TEXT REFERENCES event_template_versions (id),
  follow              TEXT NOT NULL DEFAULT 'pinned' CHECK (follow IN ('pinned', 'latest')),
  custom_json         TEXT,
  vars_json           TEXT NOT NULL DEFAULT '{}',
  effective_subject   TEXT NOT NULL,
  -- The fixed durable name (ADR-0011): the Pulse operator grants exactly this name.
  durable             TEXT NOT NULL UNIQUE,
  enabled             INTEGER NOT NULL DEFAULT 1,
  -- Cursor: a generation bump means "delete and recreate the consumer at start_policy" (SM-EVT-007).
  cursor_generation   INTEGER NOT NULL DEFAULT 1,
  applied_generation  INTEGER NOT NULL DEFAULT 0,
  start_policy        TEXT NOT NULL DEFAULT 'new' CHECK (start_policy IN ('new', 'all', 'time', 'seq')),
  start_time          INTEGER,
  start_seq           INTEGER,
  replay              INTEGER NOT NULL DEFAULT 0,
  last_event_at       INTEGER,
  version             INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  created_by          TEXT NOT NULL,
  updated_at          INTEGER NOT NULL,
  CHECK ((template_version_id IS NULL) <> (custom_json IS NULL))
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_event_subscriptions_space_name ON event_subscriptions (space_id, name);

-- Delivery history: every handled event and its outcome (bounded by housekeeping).
CREATE TABLE event_deliveries (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  subscription_id TEXT NOT NULL REFERENCES event_subscriptions (id) ON DELETE CASCADE,
  event_id        TEXT NOT NULL,
  subject         TEXT NOT NULL,
  type            TEXT NOT NULL DEFAULT '',
  stream_seq      INTEGER NOT NULL DEFAULT 0,
  outcome         TEXT NOT NULL CHECK (outcome IN ('accepted', 'duplicate', 'filtered', 'quarantined', 'replayed')),
  inbox_item_id   TEXT NOT NULL DEFAULT '',
  detail          TEXT NOT NULL DEFAULT '',
  at              INTEGER NOT NULL
) STRICT;

CREATE INDEX ix_event_deliveries_sub ON event_deliveries (subscription_id, id DESC);
CREATE INDEX ix_event_deliveries_at ON event_deliveries (at);

-- Quarantine: events that failed validation, kept (bounded) for inspection and reprocessing (SM-EVT-013).
CREATE TABLE event_quarantine (
  id              TEXT PRIMARY KEY,
  subscription_id TEXT NOT NULL REFERENCES event_subscriptions (id) ON DELETE CASCADE,
  event_id        TEXT NOT NULL,
  subject         TEXT NOT NULL,
  type            TEXT NOT NULL DEFAULT '',
  source          TEXT NOT NULL DEFAULT '',
  event_time      INTEGER NOT NULL,
  content_type    TEXT NOT NULL DEFAULT '',
  payload         BLOB NOT NULL,
  error           TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'reprocessed', 'discarded')),
  created_at      INTEGER NOT NULL,
  resolved_at     INTEGER,
  resolved_by     TEXT NOT NULL DEFAULT '',
  UNIQUE (subscription_id, event_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_event_quarantine_sub ON event_quarantine (subscription_id, status, created_at DESC);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
