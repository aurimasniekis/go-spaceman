-- Messages the notifier sent that it may edit later (ADR-0083, plan 4.2 step 7). Written in batches.

-- name: UpsertTelegramMessage :exec
INSERT INTO telegram_messages (chat_id, message_id, link_id, space_id, kind, resource_type, resource_id, resource_version, created_at, message_thread_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
-- created_at is when the message was last sent, edited or alerted: retention prunes by it, and a card loaded after a
-- restart takes it as the time up to which reminders and escalations were already announced.
-- message_thread_id is the topic a card was sent in; a write that doesn't know it keeps the one stored.
ON CONFLICT (chat_id, message_id) DO UPDATE SET resource_version = excluded.resource_version, created_at = excluded.created_at,
  message_thread_id = COALESCE(excluded.message_thread_id, telegram_messages.message_thread_id);

-- name: ListTelegramMessagesByResource :many
-- Cards and their continuation messages ('cont'; the rest of a long body, replies to the card).
SELECT chat_id, message_id, kind, resource_version, created_at, message_thread_id FROM telegram_messages
WHERE resource_type = ? AND resource_id = ? AND kind IN ('card', 'cont');

-- name: PruneTelegramMessages :execrows
-- A one-time notice (kind notice) is kept: it is what stops the notice from being sent again (ADR-0089). It is one row
-- per link at most and goes with its link.
DELETE FROM telegram_messages WHERE (chat_id, message_id) IN (
  SELECT m.chat_id, m.message_id FROM telegram_messages m WHERE m.created_at < sqlc.arg(cutoff) AND m.kind <> 'notice' LIMIT sqlc.arg(lim));

-- name: ListTelegramNoticeLinks :many
-- The links that were sent a one-time notice of a type (read once when the notifier starts).
SELECT DISTINCT link_id FROM telegram_messages WHERE kind = 'notice' AND resource_type = ?;

-- name: GetTelegramMessage :one
-- A card by its message, for a button that doesn't carry the resource (the link it was sent to binds the tap).
SELECT link_id, space_id, resource_type, resource_id, resource_version FROM telegram_messages
WHERE chat_id = ? AND message_id = ? AND kind = 'card';

-- name: DeleteTelegramMessage :exec
-- A continuation the notifier deleted (the body shrank): its row must not come back after a restart.
DELETE FROM telegram_messages WHERE chat_id = ? AND message_id = ? AND kind = 'cont';
