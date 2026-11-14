-- Telegram policy and per-space settings (ADR-0083, TG-3c).

-- name: GetTelegramSettings :one
SELECT * FROM telegram_settings WHERE singleton = 1;

-- name: InsertTelegramSettings :execrows
INSERT INTO telegram_settings (singleton, settings_json, updated_at, updated_by, version) VALUES (1, ?, ?, ?, 1) ON CONFLICT DO NOTHING;

-- name: UpdateTelegramSettings :execrows
UPDATE telegram_settings SET settings_json = sqlc.arg(settings_json), updated_at = sqlc.arg(now), updated_by = sqlc.arg(by), version = version + 1
WHERE singleton = 1 AND version = sqlc.arg(version);

-- name: DeleteTelegramSettings :execrows
DELETE FROM telegram_settings WHERE singleton = 1 AND version = ?;

-- name: GetSpaceTelegram :one
SELECT * FROM space_telegram_settings WHERE space_id = ?;

-- name: ListSpaceTelegram :many
SELECT * FROM space_telegram_settings ORDER BY space_id;

-- name: InsertSpaceTelegram :execrows
INSERT INTO space_telegram_settings (space_id, enabled, content_level, enforced_approvals, topics, terminal_watch,
  created_at, created_by, updated_at, updated_by, version)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1) ON CONFLICT DO NOTHING;

-- name: UpdateSpaceTelegram :execrows
UPDATE space_telegram_settings SET enabled = sqlc.arg(enabled), content_level = sqlc.arg(content_level),
  enforced_approvals = sqlc.arg(enforced_approvals), topics = sqlc.arg(topics), terminal_watch = sqlc.arg(terminal_watch),
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(by), version = version + 1
WHERE space_id = sqlc.arg(space_id) AND version = sqlc.arg(version);

-- name: DeleteSpaceTelegram :execrows
DELETE FROM space_telegram_settings WHERE space_id = ? AND version = ?;
