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
- A client's `allowed_scopes` default to `openid profile email offline_access` (`internal/apps/apps.go`, known scopes add `groups`).
  `groups` must be allowed explicitly; a requested but unallowed `groups` scope is dropped without an error and the token has no
  `porch.groups` (observed with `TestRealPorchOIDC`, 2026-11-03).
- Porch's `/login` page is an SPA. Its provider buttons link to `{login_url}?return_to=…` (`/login/{provider}`, `web/src/routes/login.tsx`).
  Step-up (`prompt=login` or `max_age`) redirects server-side to `/login/{provider}?intent=stepup` for upstream sessions
  (`internal/oidcop/continue.go`).

## RP-initiated logout (inspected 2026-11-03, v0.3.0 `9014040`)
- Discovery advertises `end_session_endpoint` = `<issuer>/oauth2/end_session` (`internal/oidcop/provider.go`, `PathEndSession`).
- `EndSession` (`internal/oidcop/endpoints.go`): with an `id_token_hint` (signature verified, expired accepted, `iss` must match and
  `client_id`, when given, must be in `aud`) **and** a Porch browser session of that same user, Porch revokes the session and the
  client's refresh tokens for the user (audited `oidc.end_session`). Without a hint, or without a matching session, it only redirects
  (logout-CSRF protection).
- `post_logout_redirect_uri` must be registered for the client (`post_logout_redirect_uris` on the application); `state` is passed
  through. Without a redirect URI Porch goes to `/login?logged_out=1`. There is no front- or back-channel logout at v0.3.0.
- SpaceMan (as Hallpass) does not keep ID tokens, so it sends `client_id` and `post_logout_redirect_uri` only: against Porch the
  sign-out redirects back to SpaceMan's `/login` and the Porch session stays (ADR-0045).

## How SpaceMan uses it (same as Hallpass)
- Generic OIDC client (`coreos/go-oidc/v3`). Users are identified by `(issuer, sub)`.
- `groups_claim: porch.groups` (a dotted path), mapped explicitly to SpaceMan roles and space scopes on every login.
- Porch's `authz/check` API is not used: it needs `usr_` IDs and session/automation tokens, not OIDC access tokens.

## Event naming (used by SpaceMan's outbound events, ADR-0015)
- Porch's `docs/VERSIONING.md` and `internal/outbox/outbox.go` at v0.3.0 (`9014040`): type `porch.<area>.<entity>_<action>.v1`, subject
  `evt.porch.<area>.<entity>.<action>`, name segments `^[a-z][a-z0-9_]*$`. Its relay (ADR-0043) publishes with `Nats-Msg-Id` = the
  outbox ID and `ce-time` = enqueue time. SpaceMan uses the same shape with `spaceman` in place of `porch`.
