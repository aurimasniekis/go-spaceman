-- 0054_account_pools: account pools for usage-limit failover. A space's bindings for a harness form its pool, ordered by
-- priority (lower first); an optional role pool replaces the space pool for one role.

-- +goose Up
ALTER TABLE space_accounts ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;

-- The version of a space's account configuration (its bindings, priorities and role pools), for If-Match.
ALTER TABLE spaces ADD COLUMN accounts_version INTEGER NOT NULL DEFAULT 1;

-- A role's pool: when a role has any row here, its pool is these accounts only (each is also bound to the space).
CREATE TABLE space_role_accounts (
  space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
  role       TEXT NOT NULL,
  account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
  priority   INTEGER NOT NULL DEFAULT 100,
  created_at INTEGER NOT NULL,
  created_by TEXT NOT NULL,
  PRIMARY KEY (space_id, role, account_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX ix_space_role_accounts_account ON space_role_accounts (account_id);

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
