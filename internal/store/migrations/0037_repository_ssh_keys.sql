-- 0037_repository_ssh_keys: SSH deploy keys SpaceMan holds for a space repository (ADR-0057, SM-REPO-005).
--
-- ssh_key_enc is the private key (OpenSSH format) sealed with the keyring's credential purpose; it leaves central only
-- in the wire message of a clone or fetch command and never reaches agents. ssh_public_key is what the operator adds to
-- the Git host as a read-only deploy key. known_hosts pins the host keys the node accepts for this repository (empty:
-- SpaceMan's built-in pins).

-- +goose Up
ALTER TABLE space_repositories ADD COLUMN ssh_key_enc BLOB;
ALTER TABLE space_repositories ADD COLUMN ssh_public_key TEXT NOT NULL DEFAULT '';
ALTER TABLE space_repositories ADD COLUMN ssh_key_created_at INTEGER;
ALTER TABLE space_repositories ADD COLUMN known_hosts TEXT NOT NULL DEFAULT '';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
