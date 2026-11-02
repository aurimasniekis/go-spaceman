-- 0014_skills: central, versioned skills (SM-MCP-004). A skill version is a set of text files (SKILL.md plus references)
-- materialized into a harness's own skills folder; skills give guidance, never permission, and nothing in them runs at
-- install time. Role templates and space roles list the skills their runs get.

-- +goose Up
CREATE TABLE skills (
  id          TEXT PRIMARY KEY,
  slug        TEXT NOT NULL UNIQUE,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL DEFAULT 'custom' CHECK (source IN ('custom', 'builtin')),
  source_ref  TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  created_by  TEXT NOT NULL,
  updated_at  INTEGER NOT NULL,
  version     INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE TABLE skill_versions (
  id           TEXT PRIMARY KEY,
  skill_id     TEXT NOT NULL REFERENCES skills (id) ON DELETE CASCADE,
  number       INTEGER NOT NULL,
  state        TEXT NOT NULL CHECK (state IN ('draft', 'published')),
  files_json   TEXT NOT NULL,
  notes        TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  created_by   TEXT NOT NULL,
  published_at INTEGER,
  published_by TEXT NOT NULL DEFAULT '',
  updated_at   INTEGER NOT NULL,
  version      INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_skill_versions_number ON skill_versions (skill_id, number) WHERE state = 'published';
CREATE UNIQUE INDEX ux_skill_versions_draft ON skill_versions (skill_id) WHERE state = 'draft';

-- [{"skill_id": "...", "pin": ""}]: the skills a template's or space role's runs get (pin: a published version, or follow).
ALTER TABLE role_templates ADD COLUMN skills_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE space_roles ADD COLUMN skills_json TEXT NOT NULL DEFAULT '[]';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
