-- 0072_event_fleet_scope: one designated operations space may ingest the run dialog events of every space (ADR-0106,
-- amends ADR-0104). Two parts:
--   event_fleet_scope_grants  the operator-level grant: a row names a space that may use fleet scope. Only an
--                             installation administrator writes it (system:write with step-up); a space's own
--                             permissions never reach it. Purging the space removes its grant.
--   event_subscriptions.ingest_own_scope
--                             space (default, every existing subscription) or fleet. fleet is accepted only with
--                             ingest_own on and a grant for the subscription's space, and ingestion checks the grant
--                             again for every event of another space.

-- +goose Up
ALTER TABLE event_subscriptions ADD COLUMN ingest_own_scope TEXT NOT NULL DEFAULT 'space' CHECK (ingest_own_scope IN ('space', 'fleet'));

CREATE TABLE event_fleet_scope_grants (
  space_id   TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  reason     TEXT NOT NULL DEFAULT '',
  granted_at INTEGER NOT NULL,
  granted_by TEXT NOT NULL
) STRICT, WITHOUT ROWID;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
