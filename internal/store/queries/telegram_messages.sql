-- Messages the notifier sent that it may edit later (ADR-0083, plan 4.2 step 7). Written in batches.

-- name: UpsertTelegramMessage :exec
INSERT INTO telegram_messages (chat_id, message_id, link_id, space_id, kind, resource_type, resource_id, resource_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
-- created_at is when the message was last sent, edited or alerted: retention prunes by it, and a card loaded after a
-- restart takes it as the time up to which reminders and escalations were already announced.
ON CONFLICT (chat_id, message_id) DO UPDATE SET resource_version = excluded.resource_version, created_at = excluded.created_at;

-- name: ListTelegramMessagesByResource :many
-- Cards and their continuation messages ('cont'; the rest of a long body, replies to the card).
SELECT chat_id, message_id, kind, resource_version, created_at FROM telegram_messages
WHERE resource_type = ? AND resource_id = ? AND kind IN ('card', 'cont');

-- name: PruneTelegramMessages :execrows
DELETE FROM telegram_messages WHERE (chat_id, message_id) IN (
  SELECT m.chat_id, m.message_id FROM telegram_messages m WHERE m.created_at < sqlc.arg(cutoff) LIMIT sqlc.arg(lim));

-- name: GetTelegramMessage :one
-- A card by its message, for a button that doesn't carry the resource (the link it was sent to binds the tap).
SELECT link_id, space_id, resource_type, resource_id, resource_version FROM telegram_messages
WHERE chat_id = ? AND message_id = ? AND kind = 'card';
