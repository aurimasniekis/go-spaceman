-- 0030_retention: retention, the audit log's pruned prefix, recording deletion marks and RP-initiated logout
-- (SM-REC-003, SM-HUI-007, SM-ATT-006, SM-SPACE-001, SM-OBS-003, SM-AUTH-001; ADR-0045).
--
-- Retention periods are limits (installation defaults in `limits:`, space overrides in space_limits); audit retention
-- is installation-wide (`retention.audit_days`). One daily pass applies them. Every pass is recorded here, and every
-- deletion it makes is audited and recorded in the change feed.

-- +goose Up
CREATE TABLE retention_passes (
  id           TEXT PRIMARY KEY,
  trigger      TEXT NOT NULL CHECK (trigger IN ('scheduled', 'manual')),
  requested_by TEXT NOT NULL DEFAULT '',
  state        TEXT NOT NULL CHECK (state IN ('running', 'succeeded', 'failed', 'interrupted')),
  started_at   INTEGER NOT NULL,
  finished_at  INTEGER,
  deleted_json TEXT NOT NULL DEFAULT '{}',
  error        TEXT NOT NULL DEFAULT ''
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_retention_passes_started ON retention_passes (started_at DESC);

-- The audit log's pruned prefix: every event up to through_seq was deleted by retention. through_hash is the hash of
-- the last deleted event, so the chain still verifies from the first retained event, and readers whose cursor is
-- before through_seq are told that events were pruned (a retention gap).
CREATE TABLE audit_prune (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  through_seq  INTEGER NOT NULL CHECK (through_seq > 0),
  through_hash BLOB NOT NULL,
  through_at   INTEGER NOT NULL,
  pruned       INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL
) STRICT;

-- When central's copy of a run's recording was deleted (by an operator or by retention).
ALTER TABLE runs ADD COLUMN recording_deleted_at INTEGER;

CREATE INDEX ix_runs_space_ended ON runs (space_id, ended_at) WHERE ended_at IS NOT NULL;
CREATE INDEX ix_human_actions_space_updated ON human_actions (space_id, status, updated_at);

-- RP-initiated logout (OpenID Connect RP-Initiated Logout 1.0) per provider, ported from Hallpass bdd8329: when enabled
-- and the provider's discovery document advertises end_session_endpoint, signing out of an SSO session also sends the
-- browser to the provider's logout endpoint.
ALTER TABLE oidc_providers ADD COLUMN end_session_enabled INTEGER NOT NULL DEFAULT 0 CHECK (end_session_enabled IN (0, 1));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
