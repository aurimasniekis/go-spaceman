-- 0041_message_deliver_at: scheduled messages (ADR-0065). A message with deliver_at is a scheduled message to control
-- (a reminder control sets for itself): while deliver_at is in the future it is not delivered, noticed, or counted as
-- work waiting for control. It stays set after delivery, so a scheduled message stays recognisable (the run that
-- scheduled it receives it too) and its "scheduled for" time is kept.

-- +goose Up
ALTER TABLE messages ADD COLUMN deliver_at INTEGER;

CREATE INDEX ix_messages_scheduled ON messages (deliver_at) WHERE deliver_at IS NOT NULL AND state = 'stored';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
