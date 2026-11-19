-- 0066_messages_from_run: an index for the messages a run sent (the worker hand-back watchdog, ADR-0099), so reading
-- them does not scan the whole space's messages.
-- Numbers 0061-0064 belong to other branches of the same release; the migrator needs no contiguous numbers.

-- +goose Up
CREATE INDEX ix_messages_from ON messages (space_id, from_kind, from_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
