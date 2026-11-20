-- 0070_subscription_ingest_own: a subscription may opt in to ingesting this installation's own events (ingest_own,
-- ADR-0104). Off for every existing subscription, so nothing changes until an operator opts in. The service accepts
-- the opt-in only for an exact subject on the hard-coded allow-list (the run dialog events), and ingestion admits an
-- own event only at automation depth 0; the column itself carries no rule beyond being a boolean.

-- +goose Up
ALTER TABLE event_subscriptions ADD COLUMN ingest_own INTEGER NOT NULL DEFAULT 0 CHECK (ingest_own IN (0, 1));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
