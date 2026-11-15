-- name: InsertTelegramLinkCode :exec
INSERT INTO telegram_link_codes (code_hash, user_id, created_session_method, created_session_id, created_at, expires_at, state)
VALUES (?, ?, ?, ?, ?, ?, 'new');

-- name: CancelOpenTelegramLinkCodes :execrows
-- A new code replaces the user's unfinished ones.
UPDATE telegram_link_codes SET state = 'cancelled'
WHERE user_id = sqlc.arg(user_id) AND state IN ('new', 'redeemed');

-- name: GetTelegramLinkCode :one
SELECT * FROM telegram_link_codes WHERE code_hash = ?;

-- name: RedeemTelegramLinkCode :execrows
-- /start: binds an unused, unexpired code to the first Telegram user who presents it (single use).
UPDATE telegram_link_codes SET state = 'redeemed', redeemed_tg_user_id = sqlc.arg(tg_user_id),
  redeemed_tg_username = sqlc.arg(tg_username), redeemed_tg_name = sqlc.arg(tg_name), redeemed_at = sqlc.arg(now)
WHERE code_hash = sqlc.arg(code_hash) AND state = 'new' AND redeemed_tg_user_id IS NULL AND expires_at > sqlc.arg(now);

-- name: ConfirmTelegramLinkCodeInTelegram :execrows
-- [Confirm] in the bot: only the Telegram user who redeemed the code, once, before it expires.
UPDATE telegram_link_codes SET tg_confirmed_at = sqlc.arg(now)
WHERE code_hash = sqlc.arg(code_hash) AND state = 'redeemed' AND tg_confirmed_at IS NULL
  AND redeemed_tg_user_id = sqlc.arg(tg_user_id) AND expires_at > sqlc.arg(now);

-- name: CancelTelegramLinkCode :execrows
UPDATE telegram_link_codes SET state = 'cancelled'
WHERE code_hash = sqlc.arg(code_hash) AND state IN ('new', 'redeemed');

-- name: CompleteTelegramLinkCode :execrows
-- The web confirmation: the code was confirmed in Telegram and has not expired.
UPDATE telegram_link_codes SET state = 'confirmed'
WHERE code_hash = sqlc.arg(code_hash) AND state = 'redeemed' AND tg_confirmed_at IS NOT NULL AND expires_at > sqlc.arg(now);

-- name: CountLiveTelegramLinksByUser :one
SELECT count(*) FROM telegram_links WHERE user_id = ? AND state <> 'revoked';

-- name: GetLiveTelegramLinkByTgUser :one
SELECT id, user_id FROM telegram_links WHERE tg_user_id = ? AND state <> 'revoked';

-- name: InsertTelegramLink :exec
INSERT INTO telegram_links (id, user_id, bot_id, tg_user_id, tg_username, tg_name, chat_id, state, via_sso, sso_until, max_role,
  created_at, confirmed_at, reconfirm_due_at, updated_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(bot_id), sqlc.arg(tg_user_id), sqlc.arg(tg_username), sqlc.arg(tg_name),
  sqlc.arg(chat_id), 'active', sqlc.arg(via_sso), sqlc.arg(sso_until), sqlc.arg(max_role), sqlc.arg(now), sqlc.arg(now),
  sqlc.arg(reconfirm_due_at), sqlc.arg(now));

-- name: GetLatestRevokedTelegramLinkTopics :one
-- A relink of the same chat and Telegram user copies the topics of the most recently revoked link, so it reuses them
-- instead of creating duplicates (ADR-0089).
SELECT topics_json FROM telegram_links WHERE chat_id = sqlc.arg(chat_id) AND tg_user_id = sqlc.arg(tg_user_id) AND state = 'revoked'
ORDER BY revoked_at DESC, id DESC LIMIT 1;

-- name: GetTelegramLink :one
SELECT * FROM telegram_links WHERE id = ?;

-- name: MoveTelegramLinkCodeSession :exec
-- A web session was rotated (MFA, re-authentication): its unfinished link codes follow it to the new session.
UPDATE telegram_link_codes SET created_session_id = sqlc.arg(new_session_id)
WHERE created_session_id = sqlc.arg(old_session_id) AND sqlc.arg(old_session_id) <> '' AND user_id = sqlc.arg(user_id)
  AND state IN ('new', 'redeemed');

-- name: MarkTelegramLinkCodeReused :execrows
-- A different Telegram account presented a redeemed, unexpired code: it leaked. The code is cancelled and flagged so
-- the owner's page warns.
UPDATE telegram_link_codes SET state = 'cancelled', reuse_attempted_at = sqlc.arg(now)
WHERE code_hash = sqlc.arg(code_hash) AND state = 'redeemed' AND redeemed_tg_user_id <> sqlc.arg(tg_user_id) AND expires_at > sqlc.arg(now);

-- name: ListActiveTelegramLinks :many
-- What the dispatcher's in-memory cache holds.
SELECT id, user_id, tg_user_id, chat_id, reconfirm_due_at FROM telegram_links WHERE state = 'active';

-- name: ListLiveTelegramLinksByUser :many
SELECT * FROM telegram_links WHERE user_id = ? AND state <> 'revoked' ORDER BY created_at, id;

-- name: ListLiveTelegramLinks :many
SELECT * FROM telegram_links WHERE state <> 'revoked' ORDER BY created_at, id;

-- name: RevokeTelegramLink :execrows
UPDATE telegram_links SET state = 'revoked', revoked_at = sqlc.arg(now), revoked_by = sqlc.arg(revoked_by), updated_at = sqlc.arg(now),
  version = version + 1
WHERE id = sqlc.arg(id) AND state <> 'revoked';

-- name: ReconfirmTelegramLink :execrows
UPDATE telegram_links SET reconfirm_due_at = sqlc.arg(reconfirm_due_at), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND state = 'active';

-- name: SetTelegramLinkTopics :execrows
UPDATE telegram_links SET topics_json = sqlc.arg(topics_json) WHERE id = sqlc.arg(id) AND state <> 'revoked';

-- name: SetTelegramLinkPrefs :execrows
-- A link's own preference overrides (ADR-0083 amendment TG-10-4). The link is named by the caller's authenticated
-- Telegram principal only; user_id and state repeat the service's ownership check so a mismatch writes nothing.
UPDATE telegram_links SET prefs_json = sqlc.arg(prefs_json), updated_at = sqlc.arg(now), version = version + 1
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND state = 'active' AND version = sqlc.arg(version);
