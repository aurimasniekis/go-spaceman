-- 0050_usage_limits: usage limits (ADR-0079, node protocol minor 12). Schema only; nothing writes these yet.
--
-- runs.limit_*: the limit a node reported for a run (RunReport.limit). limit_kind '' means no limit; a live run with a
-- kind is limit-waiting (keep-alive), an ended one ended with end_reason 'usage_limit' (park). limit_resets_at NULL is
-- an unknown reset; limit_since is when the limit was seen. Columns only: the runs table is not rebuilt and its CHECKs
-- are unchanged. limit_kind has no CHECK on purpose (proto.LimitKind maps an unlisted kind to 'unknown'): a new kind
-- would otherwise need a runs-table rebuild.
--
-- usage_limits: one row per exhausted account window as seen by one space. The hold it places applies to every space
-- bound to the same account (or gateway reference); control is told per space. At most one open row per space, scope,
-- account or gateway reference and kind. An account row has an account_id and no gateway_ref, a gateway row the reverse.
-- check_at is when an open hold is next checked: an unknown reset is probed (one resume or launch let through, with
-- backoff), a gateway hold re-reads the quota board (ADR-0079 decisions 5, 7 and 9).
--
-- limit_waits: one row per parked run that should come back as a new run of the same instance. last_error is a fixed
-- reason, never raw text. At most one wait per run, and one active (waiting or resuming) wait per instance.
--
-- Times are Unix microseconds like every other column. No transcript text is stored anywhere.

-- +goose Up
ALTER TABLE runs ADD COLUMN limit_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN limit_resets_at INTEGER;
ALTER TABLE runs ADD COLUMN limit_since INTEGER;

CREATE TABLE usage_limits (
  id                  TEXT PRIMARY KEY,
  space_id            TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  scope               TEXT NOT NULL CHECK (scope IN ('account', 'gateway')),
  account_id          TEXT NOT NULL DEFAULT '',
  gateway_ref         TEXT NOT NULL DEFAULT '',
  kind                TEXT NOT NULL CHECK (kind IN ('five_hour', 'seven_day', 'seven_day_opus', 'seven_day_sonnet', 'seven_day_overage_included', 'overage',
                        'gateway_quota', 'gateway_budget', 'unknown')),
  resets_at           INTEGER,
  check_at            INTEGER,
  source              TEXT NOT NULL CHECK (source IN ('transcript', 'quota_board', 'budget_status', 'operator')),
  first_run_id        TEXT NOT NULL DEFAULT '',
  seen_at             INTEGER NOT NULL,
  cleared_at          INTEGER,
  cleared_by          TEXT NOT NULL DEFAULT '',
  notified_control_at INTEGER,
  notified_humans_at  INTEGER,
  version             INTEGER NOT NULL DEFAULT 1,
  created_at          INTEGER NOT NULL,
  updated_at          INTEGER NOT NULL,
  CHECK ((scope = 'account' AND account_id <> '' AND gateway_ref = '') OR (scope = 'gateway' AND gateway_ref <> '' AND account_id = ''))
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_usage_limits_open ON usage_limits (space_id, scope, account_id, gateway_ref, kind) WHERE cleared_at IS NULL;
-- The launch-time hold reads every open row of an account (or gateway reference) across spaces.
CREATE INDEX ix_usage_limits_account_open ON usage_limits (account_id) WHERE cleared_at IS NULL AND scope = 'account';
CREATE INDEX ix_usage_limits_gateway_open ON usage_limits (gateway_ref) WHERE cleared_at IS NULL AND scope = 'gateway';
CREATE INDEX ix_usage_limits_space ON usage_limits (space_id, seen_at);
-- The deadline jobs: open holds whose reset is due, and open holds due for a probe or a gateway re-check.
CREATE INDEX ix_usage_limits_due ON usage_limits (resets_at) WHERE cleared_at IS NULL;
CREATE INDEX ix_usage_limits_check ON usage_limits (check_at) WHERE cleared_at IS NULL;

CREATE TABLE limit_waits (
  id             TEXT PRIMARY KEY,
  space_id       TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  run_id         TEXT NOT NULL UNIQUE REFERENCES runs (id) ON DELETE CASCADE,
  task_id        TEXT NOT NULL DEFAULT '',
  run_kind       TEXT NOT NULL CHECK (run_kind IN ('control', 'worker', 'kb', 'dreamer', 'diagnostic')),
  role           TEXT NOT NULL,
  node_id        TEXT NOT NULL,
  instance_id    TEXT NOT NULL,
  harness        TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  session_id     TEXT NOT NULL DEFAULT '',
  launch_json    TEXT NOT NULL,
  usage_limit_id TEXT NOT NULL DEFAULT '',
  resume_at      INTEGER NOT NULL,
  release_at     INTEGER,
  state          TEXT NOT NULL DEFAULT 'waiting' CHECK (state IN ('waiting', 'resuming', 'resumed', 'released', 'cancelled', 'failed')),
  new_run_id     TEXT NOT NULL DEFAULT '',
  attempts       INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  version        INTEGER NOT NULL DEFAULT 1,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_limit_waits_active_instance ON limit_waits (space_id, instance_id) WHERE state IN ('waiting', 'resuming');
CREATE INDEX ix_limit_waits_due ON limit_waits (resume_at) WHERE state = 'waiting';
CREATE INDEX ix_limit_waits_release ON limit_waits (release_at) WHERE state = 'waiting' AND release_at IS NOT NULL;
CREATE INDEX ix_limit_waits_space ON limit_waits (space_id, created_at);
CREATE INDEX ix_limit_waits_task ON limit_waits (task_id) WHERE task_id <> '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
