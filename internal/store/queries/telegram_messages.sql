-- Messages the notifier sent that it may edit later (ADR-0083, plan §4.2 step 7). Written in batches.

-- name: UpsertTelegramMessage :exec
INSERT INTO telegram_messages (chat_id, message_id, link_id, space_id, kind, resource_type, resource_id, resource_version, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (chat_id, message_id) DO UPDATE SET resource_version = excluded.resource_version;

-- name: ListTelegramMessagesByResource :many
SELECT chat_id, message_id, resource_version FROM telegram_messages
WHERE resource_type = ? AND resource_id = ? AND kind = 'card';
