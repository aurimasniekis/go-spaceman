-- 0013_human_types: the human-action type catalog (SM-HTYPE-*). Custom types have immutable published versions with
-- request and response JSON Schemas (2020-12), safe UI metadata and their own outcomes; spaces enable them. Each
-- action records the exact type version it was created with (SM-HTYPE-005).
--
-- human_actions is rebuilt to allow type 'custom' and type-defined outcomes (SQLite cannot alter a CHECK constraint).
-- The writer pool has one connection, so the foreign-key pragma below applies to the statements that follow.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE human_types (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE human_type_versions (
  id                     TEXT PRIMARY KEY,
  type_id                TEXT NOT NULL REFERENCES human_types (id) ON DELETE CASCADE,
  number                 INTEGER NOT NULL,
  state                  TEXT NOT NULL CHECK (state IN ('draft', 'published')),
  request_schema         TEXT NOT NULL DEFAULT '{"type":"object"}',
  response_schema        TEXT NOT NULL DEFAULT '{"type":"object"}',
  ui_json                TEXT NOT NULL DEFAULT '{}',
  outcomes_json          TEXT NOT NULL DEFAULT '["done"]',
  default_priority       TEXT NOT NULL DEFAULT 'normal',
  default_expiry_minutes INTEGER NOT NULL DEFAULT 0,
  notes                  TEXT NOT NULL DEFAULT '',
  created_at             INTEGER NOT NULL,
  created_by             TEXT NOT NULL,
  published_at           INTEGER,
  published_by           TEXT NOT NULL DEFAULT '',
  updated_at             INTEGER NOT NULL,
  version                INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_human_type_versions_number ON human_type_versions (type_id, number) WHERE state = 'published';
CREATE UNIQUE INDEX ux_human_type_versions_draft ON human_type_versions (type_id) WHERE state = 'draft';

-- A space enables a type (following its latest version or pinned to one), optionally as a variant with its own label
-- and defaults (SM-HTYPE-002).
CREATE TABLE space_human_types (
  space_id               TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  type_id                TEXT NOT NULL REFERENCES human_types (id) ON DELETE CASCADE,
  version_pin            TEXT NOT NULL DEFAULT '',
  label                  TEXT NOT NULL DEFAULT '',
  default_priority       TEXT NOT NULL DEFAULT '',
  default_expiry_minutes INTEGER NOT NULL DEFAULT 0,
  created_at             INTEGER NOT NULL,
  created_by             TEXT NOT NULL,
  PRIMARY KEY (space_id, type_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE human_actions_new (
  id                TEXT PRIMARY KEY,
  space_id          TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  type              TEXT NOT NULL CHECK (type IN ('question', 'approval', 'todo', 'message', 'custom')),
  type_version      INTEGER NOT NULL DEFAULT 1,
  type_version_id   TEXT NOT NULL DEFAULT '',
  type_slug         TEXT NOT NULL DEFAULT '',
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
  outcome           TEXT NOT NULL DEFAULT '',
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

INSERT INTO human_actions_new (id, space_id, type, type_version, title, body, request_json, requester_kind, requester_id, requester_role,
  requester_run, requester_session, requester_label, task_id, inbox_item_id, correlation_id, dedup_key, blocks_task, priority, rank, due_at,
  expires_at, assignee, status, outcome, response_json, responded_by, responded_at, status_reason, follow_up_of, created_at, created_by,
  updated_at, version)
SELECT id, space_id, type, type_version, title, body, request_json, requester_kind, requester_id, requester_role, requester_run,
  requester_session, requester_label, task_id, inbox_item_id, correlation_id, dedup_key, blocks_task, priority, rank, due_at, expires_at,
  assignee, status, outcome, response_json, responded_by, responded_at, status_reason, follow_up_of, created_at, created_by, updated_at, version
FROM human_actions;

DROP TABLE human_actions;
ALTER TABLE human_actions_new RENAME TO human_actions;

CREATE INDEX ix_human_space_rank ON human_actions (space_id, status, rank);
CREATE INDEX ix_human_status ON human_actions (status, space_id);
CREATE INDEX ix_human_expiry ON human_actions (expires_at) WHERE expires_at IS NOT NULL AND status IN ('open', 'in_progress');
CREATE UNIQUE INDEX ux_human_dedup ON human_actions (space_id, dedup_key) WHERE dedup_key <> '';

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
