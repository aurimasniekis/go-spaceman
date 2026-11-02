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
INSERT INTO space_accounts (space_id, account_id, harness, is_default, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?);
