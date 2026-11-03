# Sign-in and access

## First administrator

`spaceman admin bootstrap` issues the **root token** (`spaceman_rt_…`), shown once. It can create the first
administrator, through **Set up SpaceMan** in the web UI or the API. It is never given to nodes or agents, every use is
audited, and administrators can rotate or revoke it on the **Account** page. Treat it as a break-glass secret and keep
it offline.

A second `admin bootstrap` is refused once a root token was issued. If the root token is lost, run
`spaceman admin root-token rotate` on the server with central stopped: it revokes the active root token and prints a
new one. Once an administrator exists, `spaceman admin recover` (below) is the way back in.

<Shot name="setup-bootstrap" alt="Setup page asking for the root token, name, email and password" caption="Creating the first administrator" />

## Signing in

People sign in with a provider such as Porch ([Single sign-on](/guide/sign-in)) or with a local password. Local
accounts sign in with email and password. Accounts that hold an operator or admin grant must use an
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

## When access ends

Every request is checked when it arrives, so a revoked session, token or grant is refused from the next request on.
Open connections follow this policy:

| Change | Open live terminals and live updates |
|---|---|
| Sign out, a session revoked on the **Account** page, an API token or the root token revoked or rotated, a user disabled, a password changed or reset, an authenticator changed, grants changed | End at once. The browser checks its session and returns to sign-in if it is gone. |
| A user's grant for a space removed | That space's live updates stop at once. Its live terminals end and show **Access lost**. |
| A session expires or idles out, an API token expires, a change made with central stopped (`spaceman admin`) | End within `auth.stream_revalidate` (default 30 s, at most 60 s). |
| A node disabled, revoked or re-enrolled | Its connection closes at once. |
| A node credential expires | Its connection closes within `auth.stream_revalidate`, and the node cannot reconnect with it. |
| A run stopped or ended | Its run credential is revoked, and the run's next tool call fails. |

An open stream never counts as activity, so a tab left open cannot keep an idle session alive. Each open stream
re-checks its session or token once per interval, and once more when a matching revocation is committed.

**Revocation never undoes completed actions.** Users, runs, tasks, messages and approvals created with a credential
remain after the credential is revoked, and the audit log still attributes them to it. Runs keep running when the
person who launched them loses access. To stop work, stop the run, pause the space or disable the node. A download
that has already started finishes.

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
