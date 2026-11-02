-- 0016_approvals: enforced approvals (SM-APPR-001…005, ADR-0024). A policy makes an operation SpaceMan controls (a
-- gateway tool or a built-in agent tool) need a person's approval. An approval binds the operation, its resource, the
-- space, the requesting run and a digest of the arguments, with an expiry and a number of uses. Executions are recorded
-- separately from the decision.

-- +goose Up
CREATE TABLE approval_policies (
  id                  TEXT PRIMARY KEY,
  space_id            TEXT NOT NULL DEFAULT '',
  operation           TEXT NOT NULL,
  uses                INTEGER NOT NULL DEFAULT 1 CHECK (uses BETWEEN 1 AND 100),
  valid_seconds       INTEGER NOT NULL DEFAULT 3600,
  request_ttl_seconds INTEGER NOT NULL DEFAULT 86400,
  note                TEXT NOT NULL DEFAULT '',
  created_at          INTEGER NOT NULL,
  created_by          TEXT NOT NULL,
  UNIQUE (space_id, operation)
) STRICT, WITHOUT ROWID;

CREATE TABLE approvals (
  id                 TEXT PRIMARY KEY,
  space_id           TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  policy_id          TEXT NOT NULL,
  operation          TEXT NOT NULL,
  resource           TEXT NOT NULL,
  args_digest        TEXT NOT NULL,
  args_preview       TEXT NOT NULL,
  requester_run      TEXT NOT NULL,
  requester_role     TEXT NOT NULL,
  human_action_id    TEXT NOT NULL DEFAULT '',
  state              TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'revoked')),
  state_reason       TEXT NOT NULL DEFAULT '',
  uses_allowed       INTEGER NOT NULL,
  uses_left          INTEGER NOT NULL CHECK (uses_left >= 0),
  valid_seconds      INTEGER NOT NULL,
  decided_by         TEXT NOT NULL DEFAULT '',
  decided_by_type    TEXT NOT NULL DEFAULT '',
  decided_via        TEXT NOT NULL DEFAULT '',
  decided_at         INTEGER,
  valid_until        INTEGER,
  request_expires_at INTEGER NOT NULL,
  created_at         INTEGER NOT NULL,
  updated_at         INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX approvals_key ON approvals (space_id, operation, args_digest, requester_run);
CREATE INDEX approvals_action ON approvals (human_action_id);
CREATE INDEX approvals_space ON approvals (space_id, created_at);

CREATE TABLE approval_executions (
  id          TEXT PRIMARY KEY,
  approval_id TEXT NOT NULL REFERENCES approvals (id) ON DELETE CASCADE,
  state       TEXT NOT NULL CHECK (state IN ('executing', 'succeeded', 'failed', 'interrupted')),
  error       TEXT NOT NULL DEFAULT '',
  started_at  INTEGER NOT NULL,
  finished_at INTEGER
) STRICT, WITHOUT ROWID;

CREATE INDEX approval_executions_approval ON approval_executions (approval_id, started_at);
CREATE INDEX approval_executions_state ON approval_executions (state);

-- +goose Down
DROP TABLE approval_executions;
DROP TABLE approvals;
DROP TABLE approval_policies;
