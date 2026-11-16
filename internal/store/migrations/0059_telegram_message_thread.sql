-- 0059_telegram_message_thread: the topic a card was sent in.
--   message_thread_id  the message_thread_id the card's send reported (0: the main chat; NULL: not known, a row from
--                      before this migration). A long card's continuation messages follow the card into this thread,
--                      whatever the chat layout is by then.

-- +goose Up
ALTER TABLE telegram_messages ADD COLUMN message_thread_id INTEGER;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
