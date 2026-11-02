-- 0018_kb: knowledge-base repositories and per-requester support sessions (SM-KB-*, SM-CTRL-005, ADR-0026). A KB is a
-- Git repository in the space; each requesting agent instance gets its own support session (a kb run with its own
-- worktree, conversation and instance), allocated within the KB's limits. Requests and answers are durable and
-- correlated; proposals are accepted by review or serially, never by force.

-- +goose Up
CREATE TABLE kb_repos (
  id                      TEXT PRIMARY KEY,
  space_id                TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  slug                    TEXT NOT NULL,
  name                    TEXT NOT NULL,
  description             TEXT NOT NULL DEFAULT '',
  repo                    TEXT NOT NULL,
  base_branch             TEXT NOT NULL DEFAULT 'main',
  space_role              TEXT NOT NULL,
  entry_instructions      TEXT NOT NULL DEFAULT '',
  validator_json          TEXT NOT NULL DEFAULT '[]',
  write_policy            TEXT NOT NULL DEFAULT 'review' CHECK (write_policy IN ('read_only', 'review', 'serialized')),
  allowed_roles_json      TEXT NOT NULL DEFAULT '["*"]',
  max_sessions            INTEGER NOT NULL DEFAULT 2,
  idle_sleep_seconds      INTEGER NOT NULL DEFAULT 900,
  request_timeout_seconds INTEGER NOT NULL DEFAULT 1800,
  session_ttl_seconds     INTEGER NOT NULL DEFAULT 604800,
  enabled                 INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
  created_at              INTEGER NOT NULL,
  created_by              TEXT NOT NULL,
  updated_at              INTEGER NOT NULL,
  version                 INTEGER NOT NULL DEFAULT 1,
  UNIQUE (space_id, slug)
) STRICT, WITHOUT ROWID;

CREATE TABLE kb_sessions (
  id              TEXT PRIMARY KEY,
  kb_id           TEXT NOT NULL REFERENCES kb_repos (id) ON DELETE CASCADE,
  space_id        TEXT NOT NULL,
  requester_key   TEXT NOT NULL,
  requester_role  TEXT NOT NULL,
  requester_label TEXT NOT NULL DEFAULT '',
  instance_id     TEXT NOT NULL,
  harness_session TEXT NOT NULL DEFAULT '',
  branch          TEXT NOT NULL,
  run_id          TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL CHECK (state IN ('new', 'active', 'sleeping', 'failed', 'closed')),
  failures        INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT NOT NULL DEFAULT '',
  answered        INTEGER NOT NULL DEFAULT 0,
  last_active_at  INTEGER NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (kb_id, requester_key)
) STRICT, WITHOUT ROWID;

CREATE INDEX kb_sessions_run ON kb_sessions (run_id);

CREATE TABLE kb_requests (
  id                 TEXT PRIMARY KEY,
  kb_id              TEXT NOT NULL REFERENCES kb_repos (id) ON DELETE CASCADE,
  space_id           TEXT NOT NULL,
  session_id         TEXT NOT NULL,
  requester_run      TEXT NOT NULL,
  requester_instance TEXT NOT NULL,
  requester_role     TEXT NOT NULL,
  kind               TEXT NOT NULL CHECK (kind IN ('context', 'decision', 'discovery', 'proposal', 'documentation', 'consistency')),
  question           TEXT NOT NULL,
  task_id            TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL CHECK (state IN ('queued', 'dispatched', 'answered', 'failed', 'cancelled', 'timed_out')),
  message_id         TEXT NOT NULL DEFAULT '',
  answer             TEXT NOT NULL DEFAULT '',
  basis              TEXT NOT NULL DEFAULT '' CHECK (basis IN ('', 'committed', 'proposal', 'none')),
  evidence_json      TEXT NOT NULL DEFAULT '[]',
  attempts           INTEGER NOT NULL DEFAULT 0,
  error              TEXT NOT NULL DEFAULT '',
  deadline_at        INTEGER NOT NULL,
  created_at         INTEGER NOT NULL,
  dispatched_at      INTEGER,
  answered_at        INTEGER,
  updated_at         INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX kb_requests_session ON kb_requests (session_id, state, created_at);
CREATE INDEX kb_requests_kb ON kb_requests (kb_id, created_at);
CREATE INDEX kb_requests_open ON kb_requests (state, deadline_at);

CREATE TABLE kb_proposals (
  id            TEXT PRIMARY KEY,
  kb_id         TEXT NOT NULL REFERENCES kb_repos (id) ON DELETE CASCADE,
  space_id      TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  request_id    TEXT NOT NULL DEFAULT '',
  title         TEXT NOT NULL,
  summary       TEXT NOT NULL DEFAULT '',
  branch        TEXT NOT NULL,
  commit_id     TEXT NOT NULL,
  state         TEXT NOT NULL CHECK (state IN ('proposed', 'accepting', 'accepted', 'rejected', 'conflict', 'failed')),
  merged_commit TEXT NOT NULL DEFAULT '',
  detail        TEXT NOT NULL DEFAULT '',
  command_id    TEXT NOT NULL DEFAULT '',
  decided_by    TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX kb_proposals_kb ON kb_proposals (kb_id, created_at);

-- +goose Down
DROP TABLE kb_proposals;
DROP TABLE kb_requests;
DROP TABLE kb_sessions;
DROP TABLE kb_repos;
