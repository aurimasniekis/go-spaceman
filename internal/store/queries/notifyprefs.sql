-- name: GetNotificationPrefs :one
SELECT * FROM notification_prefs WHERE user_id = ?;

-- name: InsertNotificationPrefs :execrows
INSERT INTO notification_prefs (user_id, prefs_json, updated_at, version) VALUES (?, ?, ?, 1) ON CONFLICT DO NOTHING;

-- name: UpdateNotificationPrefs :execrows
UPDATE notification_prefs SET prefs_json = ?, updated_at = ?, version = version + 1 WHERE user_id = ? AND version = ?;
