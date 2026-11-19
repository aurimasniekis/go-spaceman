-- 0066_messages_from_run: an index for the messages a run sent (the worker hand-back watchdog, ADR-0099), so reading
-- them does not scan the whole space's messages.

-- +goose Up
CREATE INDEX ix_messages_from ON messages (space_id, from_kind, from_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
