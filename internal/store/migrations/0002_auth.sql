-- 0002_auth: users, local credentials, MFA, one-time tokens, web sessions, login throttling, role grants (Casbin
-- grouping rows), API tokens and the bootstrap root token (SM-AUTH-*, ADR-0004). Derived from go-hallpass 0008/0018/0025.
-- Also the API's idempotency records and durable change feed (SM-API-002).

-- +goose Up
CREATE TABLE users (
  id              TEXT PRIMARY KEY,
  email           TEXT UNIQUE COLLATE NOCASE,
  display_name    TEXT NOT NULL,
  enabled         INTEGER NOT NULL CHECK (enabled IN (0, 1)),
  provisioned_via TEXT NOT NULL CHECK (provisioned_via IN ('bootstrap', 'admin', 'invite', 'jit', 'recovery')),
  mfa_required    INTEGER NOT NULL DEFAULT 0 CHECK (mfa_required IN (0, 1)),
  version         INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL,
  created_by      TEXT NOT NULL,
  updated_at      INTEGER NOT NULL,
  disabled_at     INTEGER,
  last_login_at   INTEGER
) STRICT, WITHOUT ROWID;

CREATE TABLE user_local_credentials (
  user_id        TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  password_phc   TEXT NOT NULL,
  params_version INTEGER NOT NULL,
  changed_at     INTEGER NOT NULL,
  must_change    INTEGER NOT NULL DEFAULT 0 CHECK (must_change IN (0, 1))
) STRICT, WITHOUT ROWID;

-- role_grants are the Casbin grouping rows g(subject, role, domain). subject is "user:<usr_…>" or "token:<atk_…>" or
-- "root:<rtk_…>"; domain is a space ID or "*" (every space and global administration).
CREATE TABLE role_grants (
  subject    TEXT NOT NULL,
  role       TEXT NOT NULL CHECK (role IN ('viewer', 'operator', 'admin')),
  domain     TEXT NOT NULL,
  source     TEXT NOT NULL CHECK (source IN ('manual', 'sso', 'token', 'bootstrap')),
  source_ref TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (subject, role, domain, source, source_ref)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_role_grants_domain ON role_grants (domain);

CREATE TABLE mfa_totp (
  user_id      TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  secret_enc   BLOB NOT NULL,
  confirmed_at INTEGER,
  last_step    INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE mfa_totp_pending (
  user_id    TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
  secret_enc BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE TABLE recovery_codes (
  user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  code_hash  BLOB NOT NULL,
  used_at    INTEGER,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, code_hash)
) STRICT, WITHOUT ROWID;

CREATE TABLE one_time_tokens (
  hash       BLOB PRIMARY KEY,
  kind       TEXT NOT NULL CHECK (kind IN ('setup', 'invite', 'reset', 'recovery_login', 'link_code', 'oidc_test')),
  user_id    TEXT,
  data_json  TEXT NOT NULL DEFAULT '{}',
  expires_at INTEGER NOT NULL,
  used_at    INTEGER,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_ott_kind ON one_time_tokens (kind, expires_at);

CREATE TABLE web_sessions (
  id_hash             BLOB PRIMARY KEY,
  user_id             TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  csrf_secret         BLOB NOT NULL,
  auth_method         TEXT NOT NULL,
  auth_provider       TEXT NOT NULL DEFAULT '',
  mfa_at              INTEGER,
  auth_at             INTEGER NOT NULL,
  created_at          INTEGER NOT NULL,
  last_seen_at        INTEGER NOT NULL,
  absolute_expires_at INTEGER NOT NULL,
  ip                  TEXT NOT NULL DEFAULT '',
  user_agent          TEXT NOT NULL DEFAULT '',
  revoked_at          INTEGER,
  revoke_reason       TEXT,
  display_id          TEXT NOT NULL UNIQUE
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_web_sessions_user ON web_sessions (user_id, revoked_at);

CREATE TABLE login_throttle (
  key             TEXT PRIMARY KEY,
  failures        INTEGER NOT NULL,
  next_allowed_at INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- api_tokens act for the user who created them with their own grant set (role_grants subject "token:<id>"), which
-- must be a subset of the creator's grants when issued; every request is authorized for both the token and its creator.
CREATE TABLE api_tokens (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  user_id         TEXT NOT NULL REFERENCES users (id),
  mac             BLOB NOT NULL UNIQUE,
  hint            TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  never_expires   INTEGER NOT NULL DEFAULT 0 CHECK (never_expires IN (0, 1)),
  parent_token_id TEXT REFERENCES api_tokens (id),
  last_used_at    INTEGER,
  last_used_ip    TEXT,
  revoked_at      INTEGER,
  revoked_by      TEXT
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_api_tokens_user ON api_tokens (user_id);
CREATE INDEX ix_api_tokens_parent ON api_tokens (parent_token_id) WHERE parent_token_id IS NOT NULL;

-- root_tokens: the bootstrap root token (SM-AUTH-005). Full scope, never expires, shown once; only a keyed MAC is
-- stored. Rotation issues a new row and revokes the old one; at most one active row exists.
CREATE TABLE root_tokens (
  id           TEXT PRIMARY KEY,
  mac          BLOB NOT NULL UNIQUE,
  hint         TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  created_by   TEXT NOT NULL,
  last_used_at INTEGER,
  last_used_ip TEXT,
  revoked_at   INTEGER,
  revoked_by   TEXT
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX ux_root_tokens_active ON root_tokens ((1)) WHERE revoked_at IS NULL;

-- idempotency_keys store the outcome of idempotent API operations for 24 h, per principal and operation.
CREATE TABLE idempotency_keys (
  principal    TEXT NOT NULL,
  operation    TEXT NOT NULL,
  key          TEXT NOT NULL,
  request_hash BLOB NOT NULL,
  status       INTEGER NOT NULL,
  body         BLOB,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  PRIMARY KEY (principal, operation, key)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_idempotency_expires ON idempotency_keys (expires_at);

-- changes is the durable change feed behind API watches (SM-API-002): one row per committed change of a watched
-- resource, written in the same transaction. Consumers resume from a seq; pruning leaves a visible gap.
CREATE TABLE changes (
  seq           INTEGER PRIMARY KEY AUTOINCREMENT,
  at            INTEGER NOT NULL,
  space_id      TEXT,
  resource_type TEXT NOT NULL,
  resource_id   TEXT NOT NULL,
  action        TEXT NOT NULL CHECK (action IN ('created', 'updated', 'deleted')),
  version       INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX ix_changes_space ON changes (space_id, seq);
CREATE INDEX ix_changes_at ON changes (at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
