-- 0021_oidc: OIDC sign-in providers, group mappings, linked external identities and pending authorization states
-- (SM-AUTH-001/002, ADR-0029). Derived from go-hallpass 0010_oidc (operators → users). A group mapping grants a SpaceMan
-- role in a domain ("*" or one space); the grants it produces are role_grants rows of the subject "user:<id>#sso"
-- (source 'sso'), which apply only to SSO sessions (Hallpass SR-26, DECISIONS K-11).

-- +goose Up
CREATE TABLE oidc_providers (
  id                           TEXT PRIMARY KEY,
  slug                         TEXT NOT NULL UNIQUE,
  display_name                 TEXT NOT NULL,
  issuer                       TEXT NOT NULL,
  client_id                    TEXT NOT NULL,
  client_secret_enc            BLOB,
  scopes_json                  TEXT NOT NULL,
  groups_claim                 TEXT NOT NULL DEFAULT '',
  provisioning                 TEXT NOT NULL CHECK (provisioning IN ('preprovisioned', 'jit')),
  jit_json                     TEXT NOT NULL DEFAULT '{}',
  allow_private_network        INTEGER NOT NULL DEFAULT 0 CHECK (allow_private_network IN (0, 1)),
  allow_cross_origin_endpoints INTEGER NOT NULL DEFAULT 0 CHECK (allow_cross_origin_endpoints IN (0, 1)),
  ca_bundle_pem                TEXT NOT NULL DEFAULT '',
  stepup_fallback              TEXT NOT NULL DEFAULT 'none' CHECK (stepup_fallback IN ('none', 'local_totp')),
  acr_values                   TEXT NOT NULL DEFAULT '',
  enabled                      INTEGER NOT NULL CHECK (enabled IN (0, 1)),
  test_login_ok_at             INTEGER,
  test_login_by                TEXT,
  version                      INTEGER NOT NULL DEFAULT 1,
  created_at                   INTEGER NOT NULL,
  updated_at                   INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- A mapping in a space domain names the space (space_id = domain) so that purging the space removes it; a global
-- mapping has domain '*' and no space.
CREATE TABLE oidc_group_mappings (
  id          TEXT PRIMARY KEY,
  provider_id TEXT NOT NULL REFERENCES oidc_providers (id) ON DELETE CASCADE,
  group_value TEXT NOT NULL,
  role        TEXT NOT NULL CHECK (role IN ('viewer', 'operator', 'admin')),
  domain      TEXT NOT NULL,
  space_id    TEXT REFERENCES spaces (id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  CHECK ((domain = '*' AND space_id IS NULL) OR (space_id IS NOT NULL AND domain = space_id)),
  UNIQUE (provider_id, group_value, role, domain)
) STRICT, WITHOUT ROWID;

CREATE TABLE user_identities (
  issuer        TEXT NOT NULL,
  subject       TEXT NOT NULL,
  provider_id   TEXT NOT NULL REFERENCES oidc_providers (id) ON DELETE CASCADE,
  user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  linked_via    TEXT NOT NULL CHECK (linked_via IN ('admin', 'self_link', 'jit')),
  created_at    INTEGER NOT NULL,
  last_login_at INTEGER,
  PRIMARY KEY (issuer, subject)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_user_identities_user ON user_identities (user_id);

CREATE TABLE oidc_auth_states (
  state_hash      BLOB PRIMARY KEY,
  provider_id     TEXT NOT NULL REFERENCES oidc_providers (id) ON DELETE CASCADE,
  purpose         TEXT NOT NULL CHECK (purpose IN ('login', 'stepup', 'test', 'link')),
  nonce_hash      BLOB NOT NULL,
  verifier_enc    BLOB NOT NULL,
  binding_hash    BLOB NOT NULL,
  return_to       TEXT NOT NULL DEFAULT '/',
  user_id         TEXT,
  session_display TEXT,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_oidc_states_expiry ON oidc_auth_states (expires_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
