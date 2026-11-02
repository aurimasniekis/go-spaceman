# Contract: go-porch (OIDC provider)

- **Revision:** `github.com/aurimasniekis/go-porch` **v0.3.0**, commit `90140409c967474178fc2b161c4e2aefe99911e6` (local `../porch`), Apache-2.0.
  Inspected 2026-11-02. Code: `internal/oidcop/{provider,tokens}.go`.

## OIDC
- One issuer per installation (`PublicOrigin`), with discovery at `/.well-known/openid-configuration`.
- Scopes: `openid profile email offline_access groups`.
- Flow: `code` with `response_mode=query`, **PKCE S256 only**, RFC 9207 `iss` in the authorization response. ID tokens are ES256 by default (RS256 also supported).
- ID token claims: `iss, sub (u_…, stable, never an email), aud, azp, exp, iat, auth_time, nonce, amr, at_hash`, `name` (profile),
  `email`/`email_verified` (email), and `porch: {v:1, tenant:{id,slug}, roles?, groups?, truncated?}`.
- **`porch.roles` is always `[]` at v0.3.0** ("Roles come from the authorization engine (M5)"). `porch.groups` appears only when the
  app's `disclosure.groups` is true **and** the `groups` scope is requested.
- Clients are registered with `POST /api/v1/tenants/{tenant}/applications` or Terraform `porch_application`. Redirect URIs must match
  exactly, and the client secret is shown once.

## How SpaceMan uses it (same as Hallpass)
- Generic OIDC client (`coreos/go-oidc/v3`). Users are identified by `(issuer, sub)`.
- `groups_claim: porch.groups` (a dotted path), mapped explicitly to SpaceMan roles and space scopes on every login.
- Porch's `authz/check` API is not used: it needs `usr_` IDs and session/automation tokens, not OIDC access tokens.
