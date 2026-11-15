-- name: GetNotificationPrefs :one
SELECT * FROM notification_prefs WHERE user_id = ?;

-- name: InsertNotificationPrefs :execrows
INSERT INTO notification_prefs (user_id, prefs_json, updated_at, version) VALUES (?, ?, ?, 1) ON CONFLICT DO NOTHING;

-- name: UpdateNotificationPrefs :execrows
UPDATE notification_prefs SET prefs_json = ?, updated_at = ?, version = version + 1 WHERE user_id = ? AND version = ?;

-- name: ListNotificationPrefsOfLinkedUsers :many
-- The time zones of the users with a live Telegram link (the digest and quiet hours), in one read.
SELECT user_id, prefs_json FROM notification_prefs WHERE user_id IN (SELECT user_id FROM telegram_links WHERE state <> 'revoked');
