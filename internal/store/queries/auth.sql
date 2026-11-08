-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: InsertUser :exec
INSERT INTO users (id, email, display_name, enabled, provisioned_via, mfa_required, version, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?);

-- name: GetUser :one
SELECT * FROM users WHERE id = ?;

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY display_name, id;

-- name: UpdateUserProfile :execrows
UPDATE users SET email = ?, display_name = ?, mfa_required = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?;

-- name: SetUserEnabled :execrows
UPDATE users SET enabled = sqlc.arg(enabled), disabled_at = CASE WHEN sqlc.arg(enabled) = 1 THEN NULL ELSE sqlc.arg(now) END,
  version = version + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND enabled != sqlc.arg(enabled);

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = ? WHERE id = ?;

-- name: GetLocalCredential :one
SELECT * FROM user_local_credentials WHERE user_id = ?;

-- name: UpsertLocalCredential :exec
INSERT INTO user_local_credentials (user_id, password_phc, params_version, changed_at, must_change) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET password_phc = excluded.password_phc, params_version = excluded.params_version,
  changed_at = excluded.changed_at, must_change = excluded.must_change;

-- name: GetTOTP :one
SELECT * FROM mfa_totp WHERE user_id = ?;

-- name: SetConfirmedTOTP :exec
INSERT INTO mfa_totp (user_id, secret_enc, confirmed_at, last_step, created_at) VALUES (sqlc.arg(user_id), sqlc.arg(secret_enc), sqlc.arg(now), sqlc.arg(last_step), sqlc.arg(now))
ON CONFLICT (user_id) DO UPDATE SET secret_enc = excluded.secret_enc, confirmed_at = excluded.confirmed_at, last_step = excluded.last_step,
  created_at = excluded.created_at;

-- name: UpsertTOTPPending :exec
INSERT INTO mfa_totp_pending (user_id, secret_enc, created_at, expires_at) VALUES (?, ?, ?, ?)
ON CONFLICT (user_id) DO UPDATE SET secret_enc = excluded.secret_enc, created_at = excluded.created_at, expires_at = excluded.expires_at;

-- name: GetTOTPPending :one
SELECT * FROM mfa_totp_pending WHERE user_id = ?;

-- name: TakeTOTPPending :execrows
DELETE FROM mfa_totp_pending WHERE user_id = sqlc.arg(user_id) AND secret_enc = sqlc.arg(secret_enc) AND expires_at > sqlc.arg(now);

-- name: DeleteTOTPPending :exec
DELETE FROM mfa_totp_pending WHERE user_id = ?;

-- name: PurgeTOTPPending :execrows
DELETE FROM mfa_totp_pending WHERE expires_at < ?;

-- name: AdvanceTOTPStep :execrows
UPDATE mfa_totp SET last_step = sqlc.arg(step) WHERE user_id = sqlc.arg(user_id) AND last_step < sqlc.arg(step);

-- name: DeleteTOTP :execrows
DELETE FROM mfa_totp WHERE user_id = ?;

-- name: DeleteRecoveryCodes :exec
DELETE FROM recovery_codes WHERE user_id = ?;

-- name: InsertRecoveryCode :exec
INSERT INTO recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?);

-- name: UseRecoveryCode :execrows
UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL;

-- name: CountUnusedRecoveryCodes :one
SELECT count(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL;

-- name: InsertOneTimeToken :exec
INSERT INTO one_time_tokens (hash, kind, user_id, data_json, expires_at, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetOneTimeToken :one
SELECT * FROM one_time_tokens WHERE hash = ?;

-- name: UseOneTimeToken :execrows
UPDATE one_time_tokens SET used_at = sqlc.arg(now) WHERE hash = sqlc.arg(hash) AND used_at IS NULL AND expires_at > sqlc.arg(now);

-- name: RevokeOneTimeTokens :execrows
UPDATE one_time_tokens SET used_at = ? WHERE kind = ? AND user_id = ? AND used_at IS NULL;

-- name: ListOneTimeTokens :many
SELECT * FROM one_time_tokens WHERE kind = ? AND used_at IS NULL AND expires_at > ? ORDER BY created_at DESC;

-- name: PurgeOneTimeTokens :execrows
DELETE FROM one_time_tokens WHERE expires_at < ?;

-- name: InsertWebSession :exec
INSERT INTO web_sessions (id_hash, user_id, csrf_secret, auth_method, mfa_at, auth_at, created_at, last_seen_at,
  absolute_expires_at, ip, user_agent, display_id, auth_provider)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetWebSession :one
SELECT * FROM web_sessions WHERE id_hash = ?;

-- name: TouchWebSession :exec
UPDATE web_sessions SET last_seen_at = ? WHERE id_hash = ? AND revoked_at IS NULL;

-- name: UpdateWebSessionAuth :exec
UPDATE web_sessions SET auth_at = ?, mfa_at = coalesce(?, mfa_at) WHERE id_hash = ?;

-- name: RevokeWebSession :execrows
UPDATE web_sessions SET revoked_at = ?, revoke_reason = ? WHERE id_hash = ? AND revoked_at IS NULL;

-- name: RevokeWebSessionByDisplayID :execrows
UPDATE web_sessions SET revoked_at = ?, revoke_reason = ? WHERE display_id = ? AND user_id = ? AND revoked_at IS NULL;

-- name: RevokeUserSessions :execrows
UPDATE web_sessions SET revoked_at = sqlc.arg(now), revoke_reason = sqlc.arg(reason)
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL AND id_hash != sqlc.arg(except_hash);

-- name: ListUserSessions :many
SELECT * FROM web_sessions WHERE user_id = ? AND revoked_at IS NULL AND absolute_expires_at > ? ORDER BY last_seen_at DESC;

-- name: PurgeWebSessions :execrows
DELETE FROM web_sessions WHERE absolute_expires_at < ?;

-- name: GetThrottle :one
SELECT * FROM login_throttle WHERE key = ?;

-- name: UpsertThrottle :exec
INSERT INTO login_throttle (key, failures, next_allowed_at, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (key) DO UPDATE SET failures = excluded.failures, next_allowed_at = excluded.next_allowed_at, updated_at = excluded.updated_at;

-- name: DeleteThrottle :exec
DELETE FROM login_throttle WHERE key = ?;

-- name: ListPendingInvitations :many
SELECT * FROM one_time_tokens WHERE kind = 'invite' AND used_at IS NULL AND expires_at > sqlc.arg(now) ORDER BY created_at DESC;

-- name: RevokeInvitation :execrows
UPDATE one_time_tokens SET used_at = sqlc.arg(now)
WHERE hash = sqlc.arg(hash) AND kind = 'invite' AND used_at IS NULL AND expires_at > sqlc.arg(now);

-- name: GetInvitation :one
SELECT * FROM one_time_tokens WHERE hash = ? AND kind = 'invite';

-- name: InsertAPIToken :exec
INSERT INTO api_tokens (id, name, user_id, mac, hint, created_at, expires_at, never_expires, parent_token_id, purpose)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: GetAPITokenByMAC :one
SELECT * FROM api_tokens WHERE mac = ?;

-- name: GetAPIToken :one
SELECT * FROM api_tokens WHERE id = ?;

-- name: ListAPITokens :many
SELECT * FROM api_tokens ORDER BY revoked_at IS NOT NULL, created_at DESC;

-- name: RevokeAPIToken :execrows
UPDATE api_tokens SET revoked_at = sqlc.arg(now), revoked_by = sqlc.arg(actor) WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: ListActiveChildAPITokens :many
SELECT id FROM api_tokens WHERE parent_token_id = sqlc.arg(parent) AND revoked_at IS NULL ORDER BY id;

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = sqlc.arg(now), last_used_ip = sqlc.arg(ip)
WHERE id = sqlc.arg(id) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(since));

-- name: ListAPITokensByUser :many
SELECT * FROM api_tokens WHERE user_id = ? ORDER BY revoked_at IS NOT NULL, created_at DESC;

-- name: CountEnabledGlobalAdmins :one
SELECT count(DISTINCT u.id) FROM users u JOIN role_grants g ON g.subject = 'user:' || u.id
WHERE u.enabled = 1 AND g.role = 'admin' AND g.domain = '*';

-- name: ListRoleGrants :many
SELECT * FROM role_grants ORDER BY subject, domain, role;

-- name: ListRoleGrantsBySubject :many
SELECT * FROM role_grants WHERE subject = ? ORDER BY domain, role, source;

-- name: InsertRoleGrant :exec
INSERT INTO role_grants (subject, role, domain, source, source_ref, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING;

-- name: DeleteRoleGrantsBySource :exec
DELETE FROM role_grants WHERE subject = ? AND source = ?;

-- name: DeleteRoleGrantsBySubject :exec
DELETE FROM role_grants WHERE subject = ?;

-- name: DeleteRoleGrantsByDomain :execrows
DELETE FROM role_grants WHERE domain = ?;

-- name: InsertRootToken :exec
INSERT INTO root_tokens (id, mac, hint, created_at, created_by) VALUES (?, ?, ?, ?, ?);

-- name: GetRootTokenByMAC :one
SELECT * FROM root_tokens WHERE mac = ?;

-- name: GetActiveRootToken :one
SELECT * FROM root_tokens WHERE revoked_at IS NULL;

-- name: CountRootTokens :one
SELECT count(*) FROM root_tokens;

-- name: RevokeRootTokens :execrows
UPDATE root_tokens SET revoked_at = sqlc.arg(now), revoked_by = sqlc.arg(actor) WHERE revoked_at IS NULL;

-- name: TouchRootToken :exec
UPDATE root_tokens SET last_used_at = sqlc.arg(now), last_used_ip = sqlc.arg(ip)
WHERE id = sqlc.arg(id) AND (last_used_at IS NULL OR last_used_at < sqlc.arg(since));
