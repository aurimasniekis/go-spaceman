-- 0058_usage_ledger: the usage ledger of the account-switching plan (ADR-0090, plan T6). The number may change at merge
-- time, when another branch also takes it.
--
-- run_usage gains the run's account, role and harness (copied from the run, so a bucket needs no join).
--
-- usage_ledger_marks holds what the ledger has already counted: the highest cumulative per-model counters and cost estimate
-- seen so far, per harness session (or per run when it has none). A resumed run (a switch, ADR-0079) continues the same
-- native transcript, so its cumulative counters start at the earlier run's totals; counting per session keeps those out
-- of the new run's buckets. A report adds only its growth over the marks, so a repeat, a stale report that arrives late
-- and the final report never count a token twice. Usage that exists before this migration is marked as counted: it
-- predates the ledger and is not guessed into a time bucket.
--
-- usage_buckets: hourly rows (kept 90 days), rolled into daily rows (kept 730 days) by retention. account_id is '' for
-- a run without a subscription account. model is a validated model id, 'unknown' when a report named none usable.
-- account_windows: the latest rate-limit window per account and kind (the newest observation wins).
-- account_window_samples: at most one sample per account, kind and 10-minute slot, for charts (kept 30 days).

-- +goose Up
ALTER TABLE run_usage ADD COLUMN account_id TEXT NOT NULL DEFAULT '';
ALTER TABLE run_usage ADD COLUMN role TEXT NOT NULL DEFAULT '';
ALTER TABLE run_usage ADD COLUMN harness TEXT NOT NULL DEFAULT '';

UPDATE run_usage SET
  role = COALESCE((SELECT r.role FROM runs r WHERE r.id = run_usage.run_id), ''),
  harness = COALESCE((SELECT r.harness FROM runs r WHERE r.id = run_usage.run_id), ''),
  account_id = COALESCE((SELECT CASE WHEN json_valid(r.launch_json) THEN COALESCE(json_extract(r.launch_json, '$.account_ref'), '') ELSE '' END
                         FROM runs r WHERE r.id = run_usage.run_id), '');

CREATE TABLE usage_ledger_marks (
  space_id     TEXT NOT NULL,
  mark_key     TEXT NOT NULL,
  counted_json TEXT NOT NULL DEFAULT '[]',
  counted_cost REAL NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (space_id, mark_key)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_usage_ledger_marks_updated ON usage_ledger_marks (updated_at);

-- The counters of existing usage, one mark per session (the run whose report was latest) or per run without one. The
-- run_usage models are proto.ModelUsage objects; the mark keeps its own shorter keys.
INSERT INTO usage_ledger_marks (space_id, mark_key, counted_json, counted_cost, updated_at)
SELECT space_id, mark_key,
  COALESCE((SELECT json_group_array(json_object('model', COALESCE(json_extract(m.value, '$.model'), 'unknown'),
      'in', COALESCE(json_extract(m.value, '$.input_tokens'), 0), 'out', COALESCE(json_extract(m.value, '$.output_tokens'), 0),
      'cw', COALESCE(json_extract(m.value, '$.cache_write_tokens'), 0), 'cr', COALESCE(json_extract(m.value, '$.cache_read_tokens'), 0),
      'req', COALESCE(json_extract(m.value, '$.requests'), 0)))
    FROM json_each(CASE WHEN json_valid(models_json) THEN models_json ELSE '[]' END) m), '[]'),
  CASE WHEN json_valid(estimate_json) THEN COALESCE(json_extract(estimate_json, '$.usd'), 0) ELSE 0 END, updated_at
FROM (
  SELECT u.space_id, CASE WHEN r.session_id <> '' THEN r.session_id ELSE 'run:' || u.run_id END AS mark_key, u.models_json, u.estimate_json, u.updated_at,
    row_number() OVER (PARTITION BY u.space_id, CASE WHEN r.session_id <> '' THEN r.session_id ELSE 'run:' || u.run_id END ORDER BY u.updated_at DESC, u.run_id DESC) AS rn
  FROM run_usage u JOIN runs r ON r.id = u.run_id WHERE u.source = 'claude-transcript'
) WHERE rn = 1;

CREATE TABLE usage_buckets (
  space_id            TEXT NOT NULL,
  account_id          TEXT NOT NULL,
  role                TEXT NOT NULL,
  harness             TEXT NOT NULL,
  model               TEXT NOT NULL,
  granularity         TEXT NOT NULL CHECK (granularity IN ('hour', 'day')),
  bucket_start        INTEGER NOT NULL,
  input_tokens        INTEGER NOT NULL DEFAULT 0,
  output_tokens       INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
  requests            INTEGER NOT NULL DEFAULT 0,
  cost_usd            REAL NOT NULL DEFAULT 0,
  PRIMARY KEY (space_id, account_id, role, harness, model, granularity, bucket_start)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_usage_buckets_time ON usage_buckets (granularity, bucket_start);
CREATE INDEX ix_usage_buckets_account ON usage_buckets (account_id, granularity, bucket_start);

CREATE TABLE account_windows (
  account_id  TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('five_hour', 'seven_day')),
  used_pct    REAL NOT NULL,
  resets_at   INTEGER,
  observed_at INTEGER NOT NULL,
  run_id      TEXT NOT NULL DEFAULT '',
  updated_at  INTEGER NOT NULL,
  PRIMARY KEY (account_id, kind)
) STRICT, WITHOUT ROWID;

CREATE TABLE account_window_samples (
  account_id  TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('five_hour', 'seven_day')),
  slot        INTEGER NOT NULL,
  used_pct    REAL NOT NULL,
  resets_at   INTEGER,
  observed_at INTEGER NOT NULL,
  run_id      TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (account_id, kind, slot)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_account_window_samples_time ON account_window_samples (observed_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
