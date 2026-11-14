-- 0052_telegram_link_confirm: the two link-code facts the linking protocol (ADR-0083 decision 2, TG-4b) needs and
-- 0051 lacks.
--   tg_confirmed_at     when the Telegram user who redeemed the code with /start tapped [Confirm] in the bot (NULL
--                       until then). The web confirmation is offered only after it.
--   created_session_id  the display ID of the web session that created the code: only that session may confirm the
--                       link. A session rotation (MFA, re-authentication) carries it to the new session; a logout,
--                       revocation or new sign-in does not. '' (every row created before this migration) matches no
--                       session, so no older code can be confirmed.

-- +goose Up
ALTER TABLE telegram_link_codes ADD COLUMN tg_confirmed_at INTEGER;
ALTER TABLE telegram_link_codes ADD COLUMN created_session_id TEXT NOT NULL DEFAULT '';
CREATE INDEX telegram_link_codes_session ON telegram_link_codes (created_session_id) WHERE state IN ('new', 'redeemed');

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
