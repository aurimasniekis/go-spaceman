-- 0040_harbor_pilot: the Harbor Pilot gateway (ADR-0060, SM-GW-001…005). One connection per central holds the gateway's
-- base URL, a parent token with tokens:mint and an optional read-only service-principal token, both sealed with the
-- keyring's credential purpose (associated data harborpilot:parent:<id> and harborpilot:sp:<id>) and never readable
-- through any API. A space opts in per harness with its Harbor Pilot project. Run child tokens and the durable
-- revocation queue are used by the run-token lifecycle (HP-T3).

-- +goose Up
CREATE TABLE harbor_pilot_connection (
  id                    TEXT PRIMARY KEY,
  -- One connection per central.
  singleton             INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1),
  base_url              TEXT NOT NULL,
  -- The URL runs reach the gateway on ('' = base_url): it carries their child tokens.
  run_base_url          TEXT NOT NULL DEFAULT '',
  allow_insecure_http   INTEGER NOT NULL DEFAULT 0 CHECK (allow_insecure_http IN (0, 1)),
  parent_token_enc      BLOB NOT NULL,
  -- Incremented whenever a new parent token is written (the write-only version counter).
  parent_token_version  INTEGER NOT NULL DEFAULT 1,
  -- NULL: no service-principal token.
  service_token_enc     BLOB,
  service_token_version INTEGER NOT NULL DEFAULT 0,
  -- Learnt from the validation mint: the parent's client binding and its expiry (NULL: none seen).
  binding_id            TEXT NOT NULL DEFAULT '',
  parent_expires_at     INTEGER,
  validated_at          INTEGER NOT NULL,
  created_at            INTEGER NOT NULL,
  created_by            TEXT NOT NULL,
  updated_at            INTEGER NOT NULL,
  updated_by            TEXT NOT NULL,
  version               INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE space_harbor_pilot (
  space_id                     TEXT PRIMARY KEY REFERENCES spaces (id) ON DELETE CASCADE,
  enabled                      INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0, 1)),
  project_id                   TEXT NOT NULL DEFAULT '',
  -- JSON array of harness kinds routed through Harbor Pilot; the others keep their direct accounts.
  harnesses_json               TEXT NOT NULL DEFAULT '[]',
  ttl_seconds                  INTEGER NOT NULL DEFAULT 86400 CHECK (ttl_seconds BETWEEN 900 AND 604800),
  on_budget_status_unavailable TEXT NOT NULL DEFAULT 'proceed' CHECK (on_budget_status_unavailable IN ('proceed', 'refuse')),
  -- JSON array; empty: any model the binding's plan routes.
  allowed_model_aliases_json   TEXT NOT NULL DEFAULT '[]',
  created_at                   INTEGER NOT NULL,
  created_by                   TEXT NOT NULL,
  updated_at                   INTEGER NOT NULL,
  updated_by                   TEXT NOT NULL,
  version                      INTEGER NOT NULL DEFAULT 1,
  CHECK (enabled = 0 OR project_id <> '')
);

-- A run's child token (HP-T3): sealed with harborpilot:run:<run id> so a dispatch resend reuses it; token_enc is cleared
-- once the token is revoked or has expired.
CREATE TABLE run_gateway_credentials (
  run_id        TEXT PRIMARY KEY REFERENCES runs (id) ON DELETE CASCADE,
  space_id      TEXT NOT NULL,
  credential_id TEXT NOT NULL UNIQUE,
  token_enc     BLOB,
  expires_at    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);

-- Durable revocation queue (HP-T3): written in the end path's own transaction, so a crash cannot lose a revocation. Not
-- tied to runs by a foreign key, so purging a space cannot drop pending revocations.
CREATE TABLE gateway_revocations (
  credential_id   TEXT PRIMARY KEY,
  run_id          TEXT NOT NULL DEFAULT '',
  space_id        TEXT NOT NULL DEFAULT '',
  reason          TEXT NOT NULL,
  expires_at      INTEGER NOT NULL,
  state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'revoked', 'expired', 'failed')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT NOT NULL DEFAULT '',
  requested_at    INTEGER NOT NULL,
  done_at         INTEGER
);
CREATE INDEX gateway_revocations_due ON gateway_revocations (state, next_attempt_at);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
