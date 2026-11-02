-- 0008_human: the human-action queue (SM-HUM-*): questions, approvals, to-dos and messages for people, separate from the
-- inbox, the execution queue and agent messaging. Lifecycle (status) is separate from outcome (SM-HUM-005).

-- +goose Up
CREATE TABLE human_actions (
  id                TEXT PRIMARY KEY,
  space_id          TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  type              TEXT NOT NULL CHECK (type IN ('question', 'approval', 'todo', 'message')),
  type_version      INTEGER NOT NULL DEFAULT 1,
  title             TEXT NOT NULL,
  body              TEXT NOT NULL DEFAULT '',
  request_json      TEXT NOT NULL DEFAULT '{}',
  requester_kind    TEXT NOT NULL CHECK (requester_kind IN ('agent', 'operator', 'system')),
  requester_id      TEXT NOT NULL DEFAULT '',
  requester_role    TEXT NOT NULL DEFAULT '',
  requester_run     TEXT NOT NULL DEFAULT '',
  requester_session TEXT NOT NULL DEFAULT '',
  requester_label   TEXT NOT NULL DEFAULT '',
  task_id           TEXT NOT NULL DEFAULT '',
  inbox_item_id     TEXT NOT NULL DEFAULT '',
  correlation_id    TEXT NOT NULL DEFAULT '',
  dedup_key         TEXT NOT NULL DEFAULT '',
  blocks_task       INTEGER NOT NULL DEFAULT 0,
  priority          TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
  rank              INTEGER NOT NULL,
  due_at            INTEGER,
  expires_at        INTEGER,
  assignee          TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'in_progress', 'resolved', 'cancelled', 'expired')),
  outcome           TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('', 'answered', 'approved', 'denied', 'completed', 'acknowledged')),
  response_json     TEXT NOT NULL DEFAULT '{}',
  responded_by      TEXT NOT NULL DEFAULT '',
  responded_at      INTEGER,
  status_reason     TEXT NOT NULL DEFAULT '',
  follow_up_of      TEXT NOT NULL DEFAULT '',
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL,
  version           INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_human_space_rank ON human_actions (space_id, status, rank);
CREATE INDEX ix_human_status ON human_actions (status, space_id);
CREATE INDEX ix_human_expiry ON human_actions (expires_at) WHERE expires_at IS NOT NULL AND status IN ('open', 'in_progress');
CREATE UNIQUE INDEX ux_human_dedup ON human_actions (space_id, dedup_key) WHERE dedup_key <> '';

-- Comments are discussion, never responses (SM-HUM-006).
CREATE TABLE human_comments (
  id         TEXT PRIMARY KEY,
  action_id  TEXT NOT NULL REFERENCES human_actions (id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  author     TEXT NOT NULL,
  author_label TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_human_comments_action ON human_comments (action_id, created_at);

-- Durable, correlated notifications of an outcome to the requester (SM-HUM-009). Delivery adapters (waking a sleeping
-- requester, notifying a busy one, routing to control) consume these.
CREATE TABLE human_notifications (
  id             TEXT PRIMARY KEY,
  action_id      TEXT NOT NULL REFERENCES human_actions (id) ON DELETE CASCADE,
  space_id       TEXT NOT NULL,
  target_run     TEXT NOT NULL DEFAULT '',
  target_session TEXT NOT NULL DEFAULT '',
  target_role    TEXT NOT NULL DEFAULT '',
  kind           TEXT NOT NULL CHECK (kind IN ('resolved', 'cancelled', 'expired')),
  state          TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'delivered', 'acknowledged')),
  created_at     INTEGER NOT NULL,
  delivered_at   INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_human_notifications_pending ON human_notifications (space_id, state, created_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
