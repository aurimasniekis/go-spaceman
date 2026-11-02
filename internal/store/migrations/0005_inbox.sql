-- 0005_inbox: each space's inbox of incoming requests, events and messages (SM-INBOX-*). An inbox item is not a task:
-- control interprets it (reply, create work, defer, reject, associate). Bodies are Markdown or JSON and are untrusted;
-- trusted metadata comes only from the authenticated source configuration (SM-INBOX-004).

-- +goose Up
CREATE TABLE inbox_items (
  id             TEXT PRIMARY KEY,
  space_id       TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  source_kind    TEXT NOT NULL CHECK (source_kind IN ('operator', 'worker', 'human_response', 'dreamer', 'kb', 'cross_space', 'event', 'webhook')),
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

CREATE INDEX ix_inbox_space ON inbox_items (space_id, received_at DESC, id DESC);
CREATE INDEX ix_inbox_space_status ON inbox_items (space_id, status, received_at DESC);
-- At most one item per source deduplication key (duplicate events never duplicate inbox items, SM-EVT-012).
CREATE UNIQUE INDEX ux_inbox_dedup ON inbox_items (space_id, source_kind, dedup_key) WHERE dedup_key <> '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
