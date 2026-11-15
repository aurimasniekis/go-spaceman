-- 0043_harbor_pilot_usage: per-run usage read back from Harbor Pilot (ADR-0060 decision 7, HP-T5). When a gateway run
-- ends, its end path's transaction queues a row here next to the token's revocation; central then reads
-- GET /api/v1/usage/summary?ref=spaceman.run:<run>&binding_id=<binding> a few times (Harbor Pilot writes its ledger as
-- requests finish) and keeps the figures, labelled as gateway-observed. They fill run_usage with source harbor-pilot
-- only where the harness reported nothing (Codex); a harness's own usage is never overwritten.

-- +goose Up
CREATE TABLE run_gateway_usage (
  run_id          TEXT PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  space_id        TEXT NOT NULL,
  -- pending: more reads are due; final: the last read is stored; failed: Harbor Pilot refused or kept failing;
  -- unavailable: the connection has no service-principal token (or none at all), so nothing can be read.
  state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'final', 'failed', 'unavailable')),
  -- When the run's token was given up (its revocation queued): reads are timed from here.
  ended_at        INTEGER NOT NULL,
  -- Successful reads so far, and failed tries since the last success.
  reads           INTEGER NOT NULL DEFAULT 0,
  failures        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  -- The last successful read (NULL: none yet): per-model tokens (JSON array of proto.ModelUsage), Harbor Pilot's cost
  -- totals (JSON array of {kind, currency, amount}), its attempts and the attempts whose usage was not reported.
  read_at         INTEGER,
  models_json     TEXT NOT NULL DEFAULT '[]',
  costs_json      TEXT NOT NULL DEFAULT '[]',
  attempts        INTEGER NOT NULL DEFAULT 0,
  unknown_usage   INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT NOT NULL DEFAULT '',
  updated_at      INTEGER NOT NULL
) STRICT;
CREATE INDEX run_gateway_usage_due ON run_gateway_usage (state, next_attempt_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
