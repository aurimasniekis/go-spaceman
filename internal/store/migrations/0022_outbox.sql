-- 0022_outbox: events SpaceMan publishes to Pulse (SM-OBS-004/005, ADR-0015, ADR-0030). Derived from go-hallpass
-- 0030_event_outbox (Apache-2.0, same author). A row is written in the transaction of the change it describes, only while
-- outbound publication is enabled, and a relay publishes it after commit with id as the CloudEvents ID and Nats-Msg-Id,
-- so a retry is deduplicated by JetStream (2-minute window) and by consumers beyond it. Payloads are the versioned event
-- catalog of internal/outbox: identifiers and states only, never titles, bodies, requests, responses or secrets.
-- correlation_id and causation_id travel as the SpaceMan extension headers spaceman-correlation-id and
-- spaceman-causation-id (Pulse defines neither).

-- +goose Up
CREATE TABLE event_outbox (
  id              TEXT PRIMARY KEY,
  subject         TEXT NOT NULL,
  type            TEXT NOT NULL,
  space_id        TEXT NOT NULL DEFAULT '',
  correlation_id  TEXT NOT NULL DEFAULT '',
  causation_id    TEXT NOT NULL DEFAULT '',
  payload_json    TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  published_at    INTEGER,
  dead_at         INTEGER,
  last_error      TEXT
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_event_outbox_due ON event_outbox (next_attempt_at, id) WHERE published_at IS NULL AND dead_at IS NULL;
CREATE INDEX ix_event_outbox_created ON event_outbox (created_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
