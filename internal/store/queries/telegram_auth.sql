-- name: GetTelegramLinkAuth :one
SELECT id, user_id, state, via_sso, sso_until, reconfirm_due_at FROM telegram_links WHERE id = ?;
