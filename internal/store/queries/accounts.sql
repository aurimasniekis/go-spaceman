-- name: InsertAccount :exec
INSERT INTO accounts (id, name, kind, description, metadata_json, secret_enc, secret_rotated_at, enabled, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAccount :one
SELECT * FROM accounts WHERE id = ?;

-- name: GetAccountByName :one
SELECT * FROM accounts WHERE name = ?;

-- name: ListAccounts :many
SELECT * FROM accounts ORDER BY name;

-- name: UpdateAccount :execrows
UPDATE accounts SET name = sqlc.arg(name), description = sqlc.arg(description), metadata_json = sqlc.arg(metadata_json), enabled = sqlc.arg(enabled),
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: RotateAccountSecret :execrows
UPDATE accounts SET secret_enc = sqlc.arg(secret_enc), secret_version = secret_version + 1, secret_rotated_at = sqlc.arg(now), version = version + 1,
  updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: DeleteAccount :execrows
DELETE FROM accounts WHERE id = ?;

-- name: ListSpaceAccounts :many
SELECT * FROM space_accounts WHERE space_id = ? ORDER BY harness, created_at;

-- name: CountAccountBindings :one
SELECT count(*) FROM space_accounts WHERE account_id = ?;

-- name: DeleteSpaceAccounts :exec
DELETE FROM space_accounts WHERE space_id = ?;

-- name: InsertSpaceAccount :exec
INSERT INTO space_accounts (space_id, account_id, harness, is_default, priority, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: BumpSpaceAccountsVersion :one
UPDATE spaces SET accounts_version = accounts_version + 1 WHERE id = ? RETURNING accounts_version;

-- name: GetSpaceAccountsVersion :one
SELECT accounts_version FROM spaces WHERE id = ?;

-- name: ListSpaceRoleAccounts :many
SELECT * FROM space_role_accounts WHERE space_id = ? ORDER BY role, priority, created_at;

-- name: DeleteSpaceRoleAccounts :exec
DELETE FROM space_role_accounts WHERE space_id = ? AND role = ?;

-- name: DeleteOrphanSpaceRoleAccounts :exec
DELETE FROM space_role_accounts WHERE space_role_accounts.space_id = sqlc.arg(space_id)
  AND space_role_accounts.account_id NOT IN (SELECT sa.account_id FROM space_accounts sa WHERE sa.space_id = sqlc.arg(space_id));

-- name: InsertSpaceRoleAccount :exec
INSERT INTO space_role_accounts (space_id, role, account_id, priority, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?);

-- name: CountLiveRunsByAccount :many
-- Live runs per account across spaces (the account a run launched with is in its launch spec).
SELECT CAST(json_extract(launch_json, '$.account_ref') AS TEXT) AS account_id, count(*) AS live FROM runs
WHERE observed IN ('pending', 'starting', 'running', 'stopping', 'unknown') AND json_extract(launch_json, '$.account_ref') <> ''
GROUP BY 1;
