-- name: GetTelegramLinkAuth :one
SELECT id, user_id, state, via_sso, sso_until FROM telegram_links WHERE id = ?;
