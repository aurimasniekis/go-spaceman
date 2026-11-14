-- name: GetTelegramBot :one
SELECT * FROM telegram_bot WHERE singleton = 1;

-- name: InsertTelegramBot :exec
INSERT INTO telegram_bot (singleton, bot_id, username, token_enc, token_version, webhook_secret_enc, hook_id, mode, enabled,
  miniapp_host, webhook_ip_allowlist_json, created_at, created_by, updated_at, updated_by)
VALUES (1, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateTelegramBot :execrows
UPDATE telegram_bot SET bot_id = sqlc.arg(bot_id), username = sqlc.arg(username), token_enc = sqlc.arg(token_enc),
  token_version = sqlc.arg(token_version), webhook_secret_enc = sqlc.arg(webhook_secret_enc), mode = sqlc.arg(mode),
  enabled = sqlc.arg(enabled), miniapp_host = sqlc.arg(miniapp_host), webhook_ip_allowlist_json = sqlc.arg(webhook_ip_allowlist_json),
  updated_at = sqlc.arg(now), updated_by = sqlc.arg(updated_by), version = version + 1
WHERE singleton = 1 AND version = sqlc.arg(version);

-- name: DeleteTelegramBot :execrows
DELETE FROM telegram_bot WHERE singleton = 1 AND version = sqlc.arg(version);

-- name: RevokeTelegramLinksOfOtherBots :execrows
-- A different bot was configured: every live link of another bot is revoked (its private chat belongs to that bot).
UPDATE telegram_links SET updated_at = sqlc.arg(now), state = 'revoked', revoked_at = sqlc.arg(now),
  revoked_by = sqlc.arg(revoked_by), version = version + 1
WHERE state <> 'revoked' AND bot_id <> sqlc.arg(bot_id);
