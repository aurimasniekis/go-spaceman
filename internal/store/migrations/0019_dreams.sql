-- 0019_dreams: the dreamer (SM-DREAM-*, ADR-0027). A dream configuration says which role memories a dreamer run
-- consolidates, whose transcripts it may read and its budgets; dream runs record each attempt, its report and whether
-- its results were checkpointed (only then does the node advance its evidence positions).

-- +goose Up
CREATE TABLE dream_configs (
  id                    TEXT PRIMARY KEY,
  space_id              TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  slug                  TEXT NOT NULL,
  name                  TEXT NOT NULL,
  space_role            TEXT NOT NULL,
  instance_id           TEXT NOT NULL,
  memory_roles_json     TEXT NOT NULL DEFAULT '["*"]',
  transcript_roles_json TEXT NOT NULL DEFAULT '["*"]',
  include_own           INTEGER NOT NULL DEFAULT 0 CHECK (include_own IN (0, 1)),
  instructions          TEXT NOT NULL DEFAULT '',
  interval_hours        INTEGER NOT NULL DEFAULT 0,
  max_minutes           INTEGER NOT NULL DEFAULT 30,
  max_tokens            INTEGER NOT NULL DEFAULT 0,
  max_files             INTEGER NOT NULL DEFAULT 20,
  max_bytes             INTEGER NOT NULL DEFAULT 2097152,
  enabled               INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  next_due_at           INTEGER,
  created_at            INTEGER NOT NULL,
  created_by            TEXT NOT NULL,
  updated_at            INTEGER NOT NULL,
  version               INTEGER NOT NULL DEFAULT 1,
  UNIQUE (space_id, slug)
) STRICT, WITHOUT ROWID;

CREATE TABLE dream_runs (
  id                TEXT PRIMARY KEY,
  config_id         TEXT NOT NULL REFERENCES dream_configs (id) ON DELETE CASCADE,
  space_id          TEXT NOT NULL,
  run_id            TEXT NOT NULL DEFAULT '',
  trigger_kind      TEXT NOT NULL CHECK (trigger_kind IN ('manual', 'schedule')),
  state             TEXT NOT NULL CHECK (state IN ('running', 'reported', 'succeeded', 'failed', 'timed_out')),
  summary           TEXT NOT NULL DEFAULT '',
  findings_json     TEXT NOT NULL DEFAULT '[]',
  no_change         INTEGER NOT NULL DEFAULT 0,
  commit_command_id TEXT NOT NULL DEFAULT '',
  detail            TEXT NOT NULL DEFAULT '',
  started_at        INTEGER NOT NULL,
  deadline_at       INTEGER NOT NULL,
  ended_at          INTEGER,
  updated_at        INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX dream_runs_config ON dream_runs (config_id, started_at);
CREATE INDEX dream_runs_run ON dream_runs (run_id);
CREATE INDEX dream_runs_state ON dream_runs (state, deadline_at);

-- +goose Down
DROP TABLE dream_runs;
DROP TABLE dream_configs;
