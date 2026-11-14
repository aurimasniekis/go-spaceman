-- The Mini App session exchange (ADR-0083, plan 3.4.2): the Telegram user's active link to the running bot. The
-- redundant revoked test lets SQLite use the partial unique index telegram_links_tg_user. (Keep this file ASCII: sqlc
-- cuts the query short after a multi-byte character in a comment.)

-- name: GetActiveTelegramLinkForUser :one
SELECT id FROM telegram_links WHERE bot_id = ? AND tg_user_id = ? AND state != 'revoked' AND state = 'active';
