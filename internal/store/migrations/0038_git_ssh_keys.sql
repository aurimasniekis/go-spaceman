-- 0038_git_ssh_keys: shared Git SSH keys (ADR-0058, SM-REPO-005). An administrator generates a key, adds its public half to
-- a Git account (for example a read-only machine user), and lets spaces use it; a space's repositories can then clone
-- with it instead of each having its own deploy key. private_enc is sealed with the keyring's credential purpose.

-- +goose Up
CREATE TABLE git_ssh_keys (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  description  TEXT NOT NULL DEFAULT '',
  public_key   TEXT NOT NULL,
  private_enc  BLOB NOT NULL,
  all_spaces   INTEGER NOT NULL DEFAULT 0 CHECK (all_spaces IN (0, 1)),
  rotated_at   INTEGER,
  created_at   INTEGER NOT NULL,
  created_by   TEXT NOT NULL,
  updated_at   INTEGER NOT NULL,
  updated_by   TEXT NOT NULL,
  version      INTEGER NOT NULL DEFAULT 1
);

-- The spaces a key may be used by when all_spaces is 0.
CREATE TABLE git_ssh_key_spaces (
  key_id   TEXT NOT NULL REFERENCES git_ssh_keys(id) ON DELETE CASCADE,
  space_id TEXT NOT NULL REFERENCES spaces(id) ON DELETE CASCADE,
  PRIMARY KEY (key_id, space_id)
);

-- A repository uses at most one key: its own (ssh_key_enc) or a shared one.
ALTER TABLE space_repositories ADD COLUMN git_key_id TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
