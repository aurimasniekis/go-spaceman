-- 0071_account_switch_back: a run moved at a usage limit goes back to its original account once that account has reset,
-- at an idle point (ADR-0105).
--   cause              'switch_back' joins 'usage_limit' and 'operator': the move back to the chain's origin account.
--   origin_account_id  the account a chain of usage-limit switches started from (the from account of its first switch);
--                      a switch-back's target. Backfilled with from_account_id for existing usage-limit switches (the
--                      migration can't tell an older chain's start); '' for the other causes.
-- The table is rebuilt to widen the CHECK; nothing references it by foreign key.

-- +goose Up
CREATE TABLE account_switches_new (
  id                TEXT PRIMARY KEY,
  space_id          TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  instance_id       TEXT NOT NULL,
  role              TEXT NOT NULL,
  run_kind          TEXT NOT NULL CHECK (run_kind IN ('control', 'worker', 'kb', 'dreamer', 'diagnostic')),
  harness           TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  from_run_id       TEXT NOT NULL,
  new_run_id        TEXT NOT NULL DEFAULT '',
  from_account_id   TEXT NOT NULL,
  to_account_id     TEXT NOT NULL DEFAULT '',
  cause             TEXT NOT NULL CHECK (cause IN ('usage_limit', 'operator', 'switch_back')),
  limit_kind        TEXT NOT NULL,
  limit_resets_at   INTEGER,
  limit_since       INTEGER,
  usage_limit_id    TEXT NOT NULL DEFAULT '',
  limit_wait_id     TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL CHECK (state IN ('pending', 'parked', 'resumed', 'succeeded', 'failed', 'fell_back', 'cancelled')),
  reason            TEXT NOT NULL DEFAULT '',
  armed_until       INTEGER,
  version           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  origin_account_id TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

INSERT INTO account_switches_new (id, space_id, instance_id, role, run_kind, harness, from_run_id, new_run_id, from_account_id, to_account_id,
  cause, limit_kind, limit_resets_at, limit_since, usage_limit_id, limit_wait_id, state, reason, armed_until, version, created_at, updated_at,
  origin_account_id)
SELECT id, space_id, instance_id, role, run_kind, harness, from_run_id, new_run_id, from_account_id, to_account_id,
  cause, limit_kind, limit_resets_at, limit_since, usage_limit_id, limit_wait_id, state, reason, armed_until, version, created_at, updated_at,
  CASE WHEN cause = 'usage_limit' THEN from_account_id ELSE '' END
FROM account_switches;

DROP TABLE account_switches;
ALTER TABLE account_switches_new RENAME TO account_switches;

CREATE UNIQUE INDEX ux_account_switches_active ON account_switches (space_id, instance_id) WHERE state IN ('pending', 'parked', 'resumed');
CREATE INDEX ix_account_switches_space ON account_switches (space_id, created_at);
CREATE INDEX ix_account_switches_instance ON account_switches (space_id, instance_id, created_at);
CREATE INDEX ix_account_switches_from_run ON account_switches (from_run_id);
CREATE INDEX ix_account_switches_new_run ON account_switches (new_run_id) WHERE new_run_id <> '';
CREATE INDEX ix_account_switches_to_account ON account_switches (to_account_id, created_at);
CREATE INDEX ix_account_switches_armed ON account_switches (armed_until) WHERE state = 'pending';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
