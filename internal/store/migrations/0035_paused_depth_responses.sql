-- 0035_paused_depth_responses: what a subscription does while its space is paused (SM-EVT-014), bounded automation
-- depth across installations (SM-EVT-016) and routed human responses as inbox items (SM-INBOX-001). ADR-0049.
--
-- event_subscriptions.while_paused: defer (default) keeps ingesting and holds back control's notification of new items
-- until the space resumes; wake keeps ingesting and notifies control at once (the behaviour before this migration);
-- stop stops the subscription's consumer while the space is paused (durable events wait in the stream).
--
-- inbox_items.notice_deferred marks an item whose control notification waits for the space to resume. depth is the
-- automation depth: how many SpaceMan ingestions led to the item (1 for an event from a non-SpaceMan source); tasks
-- created from an item (or under a parent) inherit it, and outbound events carry it in the spaceman-depth header.
-- event_quarantine.depth keeps the depth of an event quarantined for being too deep, so reprocessing it (an
-- operator's decision) still passes the depth on.
--
-- human_policies.response_inbox: off (default), routed (a final response routed to control or the operators also
-- becomes an inbox item) or always (every final response of the type does).

-- +goose Up
ALTER TABLE event_subscriptions ADD COLUMN while_paused TEXT NOT NULL DEFAULT 'defer' CHECK (while_paused IN ('defer', 'wake', 'stop'));

ALTER TABLE inbox_items ADD COLUMN notice_deferred INTEGER NOT NULL DEFAULT 0 CHECK (notice_deferred IN (0, 1));
ALTER TABLE inbox_items ADD COLUMN depth INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0);
CREATE INDEX ix_inbox_notice_deferred ON inbox_items (space_id, received_at) WHERE notice_deferred = 1;

ALTER TABLE tasks ADD COLUMN depth INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0);

ALTER TABLE event_outbox ADD COLUMN depth INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0);

ALTER TABLE event_quarantine ADD COLUMN depth INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0);

ALTER TABLE human_policies ADD COLUMN response_inbox TEXT NOT NULL DEFAULT 'off' CHECK (response_inbox IN ('off', 'routed', 'always'));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
