-- name: InsertOIDCProvider :exec
INSERT INTO oidc_providers (id, slug, display_name, issuer, client_id, client_secret_enc, scopes_json, groups_claim, provisioning, jit_json,
  allow_private_network, allow_cross_origin_endpoints, ca_bundle_pem, stepup_fallback, acr_values, end_session_enabled, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateOIDCProvider :execrows
UPDATE oidc_providers SET slug = ?, display_name = ?, issuer = ?, client_id = ?, client_secret_enc = ?, scopes_json = ?, groups_claim = ?,
  provisioning = ?, jit_json = ?, allow_private_network = ?, allow_cross_origin_endpoints = ?, ca_bundle_pem = ?, stepup_fallback = ?,
  acr_values = ?, end_session_enabled = ?, enabled = ?, test_login_ok_at = ?, test_login_by = ?, version = version + 1, updated_at = ?
WHERE id = ? AND version = ?;

-- name: GetOIDCProvider :one
SELECT * FROM oidc_providers WHERE id = ?;

-- name: GetOIDCProviderBySlug :one
SELECT * FROM oidc_providers WHERE slug = ?;

-- name: ListOIDCProviders :many
SELECT * FROM oidc_providers ORDER BY display_name, id;

-- name: DeleteOIDCProvider :execrows
DELETE FROM oidc_providers WHERE id = ?;

-- name: SetOIDCTestLogin :exec
UPDATE oidc_providers SET test_login_ok_at = ?, test_login_by = ? WHERE id = ?;

-- name: InsertOIDCMapping :exec
INSERT INTO oidc_group_mappings (id, provider_id, group_value, role, domain, space_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: DeleteOIDCMapping :execrows
DELETE FROM oidc_group_mappings WHERE id = ? AND provider_id = ?;

-- name: ListOIDCMappings :many
SELECT * FROM oidc_group_mappings WHERE provider_id = ? ORDER BY group_value, domain, role;

-- name: GetUserIdentity :one
SELECT * FROM user_identities WHERE issuer = ? AND subject = ?;

-- name: InsertUserIdentity :exec
INSERT INTO user_identities (issuer, subject, provider_id, user_id, linked_via, created_at) VALUES (?, ?, ?, ?, ?, ?);

-- name: TouchUserIdentity :exec
UPDATE user_identities SET last_login_at = ? WHERE issuer = ? AND subject = ?;

-- name: ListUserIdentities :many
SELECT * FROM user_identities WHERE user_id = ? ORDER BY created_at, issuer, subject;

-- name: CountUserIdentities :one
SELECT count(*) FROM user_identities WHERE user_id = ?;

-- name: DeleteUserIdentity :execrows
DELETE FROM user_identities WHERE issuer = ? AND subject = ? AND user_id = ?;

-- name: ListUserIDsForProvider :many
SELECT DISTINCT user_id FROM user_identities WHERE provider_id = ? ORDER BY user_id;

-- name: InsertAuthState :exec
INSERT INTO oidc_auth_states (state_hash, provider_id, purpose, nonce_hash, verifier_enc, binding_hash, return_to, user_id, session_display, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: TakeAuthState :one
DELETE FROM oidc_auth_states WHERE state_hash = ? RETURNING *;

-- name: PurgeAuthStates :execrows
DELETE FROM oidc_auth_states WHERE expires_at < ?;

-- name: CountSSOAdmins :one
-- Enabled users with a manual global admin grant and a linked identity on an enabled provider: the administrators who
-- can still sign in while password sign-in is off (SSO-only enforcement, Hallpass HP-AUTH-015). SSO-mapped grants are
-- not counted: they are re-derived at every SSO sign-in (ADR-0029).
SELECT count(DISTINCT u.id) FROM users u
JOIN role_grants g ON g.subject = 'user:' || u.id AND g.role = 'admin' AND g.domain = '*'
JOIN user_identities i ON i.user_id = u.id
JOIN oidc_providers p ON p.id = i.provider_id AND p.enabled = 1
WHERE u.enabled = 1;

-- name: CountTestedOIDCProviders :one
SELECT count(*) FROM oidc_providers WHERE enabled = 1 AND test_login_ok_at IS NOT NULL;
