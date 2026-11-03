-- 0036_codex_chatgpt_login: the account kind codex_chatgpt_login, a Codex ChatGPT-plan login (the auth.json
-- `codex login` writes), for SM-ACCT-002. ADR-0053.
--
-- accounts is rebuilt to allow the new kind (SQLite cannot alter a CHECK constraint). The columns keep their order and
-- the sealed secrets are copied as they are. space_accounts refers to accounts, so foreign keys are off while the
-- table is replaced; the writer pool has one connection, so the pragma applies to the statements that follow.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE accounts_new (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  kind              TEXT NOT NULL CHECK (kind IN ('claude_oauth_token', 'anthropic_api_key', 'openai_api_key', 'codex_chatgpt_login')),
  description       TEXT NOT NULL DEFAULT '',
  metadata_json     TEXT NOT NULL DEFAULT '{}',
  secret_enc        BLOB NOT NULL,
  secret_version    INTEGER NOT NULL DEFAULT 1,
  secret_rotated_at INTEGER NOT NULL,
  enabled           INTEGER NOT NULL DEFAULT 1,
  created_at        INTEGER NOT NULL,
  created_by        TEXT NOT NULL,
  updated_at        INTEGER NOT NULL,
  version           INTEGER NOT NULL DEFAULT 1
) STRICT, WITHOUT ROWID;

INSERT INTO accounts_new (id, name, kind, description, metadata_json, secret_enc, secret_version, secret_rotated_at, enabled, created_at,
  created_by, updated_at, version)
SELECT id, name, kind, description, metadata_json, secret_enc, secret_version, secret_rotated_at, enabled, created_at,
  created_by, updated_at, version
FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
