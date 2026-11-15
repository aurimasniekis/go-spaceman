-- 0055_account_switches: a run that reaches its usage limit moves to another account of its pool (ADR-0090, plan T3).
-- The number may change at merge time, when another branch also takes it.
--
-- account_switches: one row per switch central armed, or decided against (fell_back) when failover is on. state moves
-- pending (armed, waiting for the run's idle point until armed_until) -> parked (the run is parked with a limit wait
-- due at once) -> resumed (a new run of the same instance started on to_account_id with --resume) -> succeeded (the
-- new run finished a turn) | failed (it ended before its first turn, or hit a limit at once) | fell_back (no switch:
-- the run waits as ADR-0079 does) | cancelled (the limit cleared, or the run was stopped or replaced). reason is a
-- fixed word, never raw text. At most one active (pending, parked or resumed) switch per instance.
--
-- limit_waits.switch_to_account_id: the account a wait's resume uses instead of its launch's account (a switch's wait,
-- or the fresh retry of a switch's failed resume); account_switch_id links the wait to its switch.

-- +goose Up
CREATE TABLE account_switches (
  id              TEXT PRIMARY KEY,
  space_id        TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  instance_id     TEXT NOT NULL,
  role            TEXT NOT NULL,
  run_kind        TEXT NOT NULL CHECK (run_kind IN ('control', 'worker', 'kb', 'dreamer', 'diagnostic')),
  harness         TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  from_run_id     TEXT NOT NULL,
  new_run_id      TEXT NOT NULL DEFAULT '',
  from_account_id TEXT NOT NULL,
  to_account_id   TEXT NOT NULL DEFAULT '',
  cause           TEXT NOT NULL CHECK (cause IN ('usage_limit', 'operator')),
  limit_kind      TEXT NOT NULL,
  limit_resets_at INTEGER,
  limit_since     INTEGER,
  usage_limit_id  TEXT NOT NULL DEFAULT '',
  limit_wait_id   TEXT NOT NULL DEFAULT '',
  state           TEXT NOT NULL CHECK (state IN ('pending', 'parked', 'resumed', 'succeeded', 'failed', 'fell_back', 'cancelled')),
  reason          TEXT NOT NULL DEFAULT '',
  armed_until     INTEGER,
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_account_switches_active ON account_switches (space_id, instance_id) WHERE state IN ('pending', 'parked', 'resumed');
CREATE INDEX ix_account_switches_space ON account_switches (space_id, created_at);
CREATE INDEX ix_account_switches_instance ON account_switches (space_id, instance_id, created_at);
CREATE INDEX ix_account_switches_from_run ON account_switches (from_run_id);
CREATE INDEX ix_account_switches_new_run ON account_switches (new_run_id) WHERE new_run_id <> '';
CREATE INDEX ix_account_switches_to_account ON account_switches (to_account_id, created_at);
CREATE INDEX ix_account_switches_armed ON account_switches (armed_until) WHERE state = 'pending';

ALTER TABLE limit_waits ADD COLUMN switch_to_account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE limit_waits ADD COLUMN account_switch_id TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
