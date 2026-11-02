# Sign-in and access

## First administrator

`spaceman admin bootstrap` issues the **root token** (`spaceman_rt_…`), shown once. It can create the first
administrator, through **Set up SpaceMan** in the web UI or the API. It is never given to nodes or agents, every use is
audited, and administrators can rotate or revoke it on the **Account** page. Treat it as a break-glass secret and keep
it offline.

<Shot name="setup-bootstrap" alt="Setup page asking for the root token, name, email and password" caption="Creating the first administrator" />

## Signing in

Local accounts sign in with email and password. Accounts that hold an operator or admin grant must use an
authenticator app (TOTP), and with `auth.require_totp` (the default) every local account must. On first sign-in the user
scans a QR code, confirms a code, saves ten single-use recovery codes and then signs in with a fresh code.

<Shot name="setup-mfa" alt="Two-factor setup page with a QR code and the manual entry key" caption="Enrolling an authenticator on first sign-in" />

Sessions are cookies (`__Host-sm_session` over HTTPS) with an idle and an absolute timeout. State-changing requests carry
a CSRF token and must come from the configured public origin. Each user sees and can revoke their sessions on the
**Account** page.

**Sensitive actions ask again.** Creating users or nodes, changing grants, enabling or disabling users, issuing reset
or enrollment tokens, creating API tokens and managing the root token need a recent re-authentication with your password
and a TOTP code; changing your password needs the password. The UI prompts and retries the action once confirmed.

## Roles

Grants assign a role in one space, or in every space (`*`, global). Stronger roles include the weaker ones.

| Role | Can |
|---|---|
| **viewer** | Read spaces, runs, configuration and catalogs. Global viewers also see nodes. |
| **operator** | Everything a viewer can, plus launch and stop runs, pause and edit spaces, watch terminals and read recordings. |
| **admin** | Everything, including terminal takeover, recording deletion and secrets. In `*`: nodes, users, tokens, audit, system settings and creating spaces. |

Authorization uses [Casbin](https://casbin.org) with roles per domain. A space you cannot read looks exactly like one
that does not exist.

<Shot name="auth-users" alt="Users page with an admin, a space operator and a global viewer" caption="Users and their grants" />

## API tokens

Users create API tokens (`spaceman_at_…`) on the **API tokens** page. Each token has its own grants, every one of which
must be within the creator's own grants, and an expiry (or none). A request with a token is allowed only when both the
token and its creator are allowed, so a token loses whatever its creator loses. Tokens are shown once.

<Shot name="auth-tokens" alt="API tokens page listing terraform-ci and grafana-readonly with their grants" caption="Scoped API tokens" />

## Lost passwords and authenticators

- **Forgotten password:** an administrator issues a single-use **password reset token** from the user's menu on the
  **Users** page. The user opens `/reset`, pastes it and chooses a new password; every session of the account ends.
- **Lost authenticator:** sign in with a recovery code instead of a TOTP code. **Replace recovery codes** on the Account
  page issues a new set.
- **Locked out entirely:** on the server, with central stopped, `spaceman admin recover <user-id>` (or
  `--new-admin EMAIL`) issues a recovery token. Open `/recover` and paste it. The account's other sessions, authenticator
  and recovery codes are removed and a new password is set. Every administrator sees a red banner until one of them
  acknowledges the break-glass use.

## Audit log

Every security-relevant action is appended to a hash-chained audit log with actor, target, outcome and source address.
Secret values are never recorded. **Verify chain** recomputes the chain and reports the first broken link, if any.

<Shot name="audit-log" alt="Audit log listing run launches, space creation, node enrollment and sign-in events" caption="The audit log" />
