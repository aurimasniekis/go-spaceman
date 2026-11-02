-- 0012_catalog: prompts, harness profiles, role templates and space roles (SM-PROMPT-*). Published prompt versions are
-- immutable; profiles, templates and space roles carry a revision that run manifests record.

-- +goose Up
CREATE TABLE prompts (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('role', 'dream', 'kb', 'event', 'general')),
  description TEXT NOT NULL DEFAULT '',
  forked_from TEXT NOT NULL DEFAULT '', -- prompt version ID this prompt was forked from (provenance)
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE prompt_versions (
  id           TEXT PRIMARY KEY,
  prompt_id    TEXT NOT NULL REFERENCES prompts (id) ON DELETE CASCADE,
  number       INTEGER NOT NULL,             -- 0 while a draft; 1, 2, … once published
  state        TEXT NOT NULL CHECK (state IN ('draft', 'published')),
  body         TEXT NOT NULL,
  extends      TEXT NOT NULL DEFAULT '',     -- a published version of another prompt this one builds on
  notes        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  created_by   TEXT NOT NULL,
  published_at INTEGER,
  published_by TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL,
  version      INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_prompt_versions_number ON prompt_versions (prompt_id, number) WHERE state = 'published';
CREATE UNIQUE INDEX ux_prompt_versions_draft ON prompt_versions (prompt_id) WHERE state = 'draft';

CREATE TABLE harness_profiles (
  id            TEXT PRIMARY KEY,
  slug          TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL,
  harness       TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  model         TEXT NOT NULL DEFAULT '',
  settings_json TEXT NOT NULL DEFAULT '{}',
  description   TEXT NOT NULL DEFAULT '',
  revision      INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  created_by    TEXT NOT NULL,
  updated_at    INTEGER NOT NULL,
  version       INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE role_templates (
  id                TEXT PRIMARY KEY,
  slug              TEXT NOT NULL UNIQUE,
  name              TEXT NOT NULL,
  kind              TEXT NOT NULL CHECK (kind IN ('control', 'worker', 'kb', 'dreamer', 'diagnostic')),
  description       TEXT NOT NULL DEFAULT '',
  profile_id        TEXT NOT NULL DEFAULT '' ,
  prompt_id         TEXT NOT NULL DEFAULT '',
  prompt_pin        TEXT NOT NULL DEFAULT '', -- a published version ID, or '' to follow the latest
  memory_conventions TEXT NOT NULL DEFAULT '',
  settings_json     TEXT NOT NULL DEFAULT '{}',
  revision          INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL,
  version           INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE space_roles (
  space_id      TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  template_id   TEXT NOT NULL REFERENCES role_templates (id),
  profile_id    TEXT NOT NULL DEFAULT '',
  prompt_id     TEXT NOT NULL DEFAULT '',
  prompt_pin    TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL DEFAULT '',
  settings_json TEXT NOT NULL DEFAULT '{}',
  instructions  TEXT NOT NULL DEFAULT '',     -- space-specific text appended after the role prompt
  revision      INTEGER NOT NULL DEFAULT 1,
  created_at    INTEGER NOT NULL,
  created_by    TEXT NOT NULL,
  updated_at    INTEGER NOT NULL,
  version       INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (space_id, name)
) STRICT, WITHOUT ROWID;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
