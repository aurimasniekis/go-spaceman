-- 0009_accounts: harness accounts (SM-ACCT-*). A credential is sealed with the keyring (purpose credential) and is never
-- returned; spaces bind the accounts their runs may use, with at most one default per harness.

-- +goose Up
CREATE TABLE accounts (
  id                TEXT PRIMARY KEY,
  name              TEXT NOT NULL UNIQUE,
  kind              TEXT NOT NULL CHECK (kind IN ('claude_oauth_token', 'anthropic_api_key', 'openai_api_key')),
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

CREATE TABLE space_accounts (
  space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  harness    TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
  is_default INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (space_id, account_id)
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_space_accounts_default ON space_accounts (space_id, harness) WHERE is_default = 1;
CREATE INDEX ix_space_accounts_account ON space_accounts (account_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
