-- 0007_queue: each space's execution queue (SM-QUEUE-*). One explicit order (rank, lower first) per space; priority and
-- policy only decide where a new task is inserted, and operator moves are authoritative (manual_position).

-- +goose Up
CREATE TABLE tasks (
  id              TEXT PRIMARY KEY,
  space_id        TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  parent_id       TEXT REFERENCES tasks (id) ON DELETE CASCADE,
  title           TEXT NOT NULL,
  objective       TEXT NOT NULL DEFAULT '',
  acceptance      TEXT NOT NULL DEFAULT '',
  kind            TEXT NOT NULL DEFAULT 'ordinary' CHECK (kind IN ('ordinary', 'incident')),
  priority        TEXT NOT NULL DEFAULT 'normal' CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
  rank            INTEGER NOT NULL,
  manual_position INTEGER NOT NULL DEFAULT 0,
  status          TEXT NOT NULL DEFAULT 'backlog' CHECK (status IN ('backlog', 'ready', 'running', 'blocked', 'review', 'done', 'failed', 'cancelled')),
  status_reason   TEXT NOT NULL DEFAULT '',
  exclusions_json TEXT NOT NULL DEFAULT '[]',
  assignee_role   TEXT NOT NULL DEFAULT '',
  assignee_run    TEXT NOT NULL DEFAULT '',
  workspace       TEXT NOT NULL DEFAULT '',
  origin_kind     TEXT NOT NULL DEFAULT '',
  origin_id       TEXT NOT NULL DEFAULT '',
  dedup_key       TEXT NOT NULL DEFAULT '',
  result          TEXT NOT NULL DEFAULT '',
  preemptions     INTEGER NOT NULL DEFAULT 0,
  created_at      INTEGER NOT NULL,
  created_by      TEXT NOT NULL,
  updated_at      INTEGER NOT NULL,
  started_at      INTEGER,
  finished_at     INTEGER,
  version         INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_tasks_space_rank ON tasks (space_id, rank);
CREATE INDEX ix_tasks_parent ON tasks (parent_id);
-- A trigger (an incident event, say) creates at most one task per dedup key: duplicates never re-promote it (SM-QUEUE-002).
CREATE UNIQUE INDEX ux_tasks_dedup ON tasks (space_id, dedup_key) WHERE dedup_key <> '';

CREATE TABLE task_dependencies (
  task_id    TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  depends_on TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  PRIMARY KEY (task_id, depends_on),
  CHECK (task_id <> depends_on)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_task_dependencies_on ON task_dependencies (depends_on);

-- Explicit blockers (SM-QUEUE-006): a human action, or anything else named by control or an operator.
CREATE TABLE task_blockers (
  id          TEXT PRIMARY KEY,
  task_id     TEXT NOT NULL REFERENCES tasks (id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('human_action', 'external')),
  ref_id      TEXT NOT NULL DEFAULT '',
  note        TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  resolved_at INTEGER,
  resolved_by TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_task_blockers_task ON task_blockers (task_id, resolved_at);

CREATE TABLE queue_policies (
  space_id            TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  max_active          INTEGER NOT NULL DEFAULT 1 CHECK (max_active BETWEEN 1 AND 20),
  incident_insertion  TEXT NOT NULL DEFAULT 'top' CHECK (incident_insertion IN ('top', 'priority')),
  incident_preempts   INTEGER NOT NULL DEFAULT 0,
  escalation_reorders INTEGER NOT NULL DEFAULT 0,
  version             INTEGER NOT NULL DEFAULT 1,
  updated_at          INTEGER NOT NULL,
  updated_by          TEXT NOT NULL
) STRICT, WITHOUT ROWID;

-- Selection decisions with the reasons tasks were skipped (SM-QUEUE-005), bounded by housekeeping.
CREATE TABLE queue_decisions (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  space_id     TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  selected_id  TEXT NOT NULL DEFAULT '',
  skipped_json TEXT NOT NULL DEFAULT '[]',
  at           INTEGER NOT NULL,
  by           TEXT NOT NULL
) STRICT;

CREATE INDEX ix_queue_decisions_space ON queue_decisions (space_id, id DESC);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
