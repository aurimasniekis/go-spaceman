-- 0053_telegram_link_reuse: the leaked-code signal of the linking protocol (ADR-0083 decision 2, TG-4b).
--   reuse_attempted_at  when a different Telegram account presented a code after another had redeemed it. Only someone
--                       who saw the code can do that, so the code is cancelled at once and the owner's confirm page
--                       shows a warning instead of a plain "cancelled" (NULL: nobody did).

-- +goose Up
ALTER TABLE telegram_link_codes ADD COLUMN reuse_attempted_at INTEGER;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
