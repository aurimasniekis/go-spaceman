# Command line

The `spaceman` binary is also a client for a running central. Its remote commands call the same administrative API as the
web UI and Terraform (`/api/v1`), with an API token, so they are authorized, audited and rate-limited exactly like the UI.
Every command is bound to an operation of the [HTTP API](/reference/api) of its own build; the
[command-line reference](/reference/cli/) is generated from the command definitions and lists the operations each command
calls.

The local commands work on the server itself: `serve` runs central, and `admin`, `keys`, `backup` and `artifacts` work
on the data directory directly (offline).

## Installation

The client is the same `spaceman` binary as central (release archives, container image or `go build ./cmd/spaceman`,
see [Installation](./install)). Use the client from the same release as the server: it knows the API of its own build.

## Connecting and authenticating

| Setting | Flag | Environment |
|---|---|---|
| Central URL | `--server https://spaceman.example.com` | `SPACEMAN_SERVER` |
| API token | `--token-file PATH` (`-` reads standard input) | `SPACEMAN_TOKEN_FILE`, or the token itself in `SPACEMAN_TOKEN` |
| Output | `-o table` (default), `-o json`, `-o yaml` | `SPACEMAN_OUTPUT` |
| Default space | `--space ID-or-slug` on space-scoped commands | `SPACEMAN_SPACE` |

There is no `--token` flag: values of flags are visible to other users in the process list, so the token (and every other
secret) comes from a file, standard input or the environment. A token file must not be readable by other users
(`chmod 600`); the client refuses it otherwise.

Create a token on the **API tokens** page (see [Sign-in and access](./access#api-tokens)) with the grants the CLI needs,
or with `spaceman token create`. The bootstrap root token also works, but keep it as a break-glass credential; it is not
a user, so it cannot create API tokens.

```sh
install -m 600 /dev/null ~/.config/spaceman/token
$EDITOR ~/.config/spaceman/token              # paste spaceman_at_…
export SPACEMAN_SERVER=https://spaceman.example.com
export SPACEMAN_TOKEN_FILE=~/.config/spaceman/token
spaceman whoami
```

The rules are the same as the Terraform provider's:

- Plain `http://` is accepted only for loopback addresses (`localhost`, `127.0.0.1`, `::1`). Anything else needs `https://`,
  unless you pass `--allow-insecure-http` (or `SPACEMAN_ALLOW_INSECURE_HTTP=1`), which sends the token in clear text.
- Redirects are never followed, so the token never leaves the server you configured. A redirect is reported as an error.
- The client logs nothing and never prints request bodies.

## Examples

```sh
spaceman node create build-01 --label zone=eu --secret-file ./build-01.enroll   # enrollment token to a 0600 file
spaceman space create payments --name Payments --node build-01
spaceman space update payments --goals "Ship payouts"
spaceman inbox create --space payments --title "Rotate the signing key" --body-file note.md --urgency high
spaceman task list --space payments
spaceman human list                                   # open human actions in every space you can see
spaceman human respond --space payments hac_01… --outcome approved --note "ok for staging"
pass show anthropic | spaceman account create --name ci --kind anthropic_api_key --secret-stdin
spaceman limits set --space payments --set max_concurrent_runs=3
spaceman event subscription replay --space payments sub_01… --policy time --time 2026-11-01T00:00:00Z --reason "template fix"
spaceman audit list --action space.relocated --all -o json
spaceman task block --space payments tsk_01… --kind external --note "waiting for the vendor"
spaceman human policy set approval --remind-after-minutes 30 --escalate-after-minutes 120 --escalate-priority urgent
spaceman message send --space payments --to role:worker --subject "Freeze deploys" --body-file freeze.md --request
spaceman attachment upload --space payments ./screenshot.png        # prints the spaceman://attachment/… uri
spaceman run transcript download --space payments run_01… --out transcript.jsonl
spaceman oidc provider create porch --issuer https://porch.example.com --client-id spaceman --client-secret-file ./porch.secret
spaceman watch --after 1200 -o json                                  # the change feed as JSON Lines
```

### What the command groups cover

| Area | Commands |
|---|---|
| Fleet | `node`, `space` (with `relocate` and `usage`), `run` (with `input`, `usage`, `recording`, `transcript`) |
| Work | `inbox`, `task` (blockers, dependencies, escalation, decisions, queue policy), `schedule`, `message`, `human` (actions, `type`, `policy`), `approval` |
| Knowledge | `kb` (requests, proposals), `memory` (collections, files, history, conflicts), `dream`, `attachment` |
| Configuration | `limits`, `account` (and a space's accounts), `prompt`, `skill`, `profile`, `role`, `event` (connections, templates, subscriptions, quarantine), `mcp`, `approval-policy`, `xspace` |
| Access | `user` (grants, reset tokens, identities), `token`, `oidc` (providers, group mappings), `identity`, `session`, `mfa`, `password`, `whoami` |
| Operations | `audit`, `system` (info, backups, outbox, restore and break-glass notices, root token), `watch` |

### Relocating a space

```sh
spaceman space relocate precheck payments --to build-02          # inspection only; nothing changes
spaceman space relocate start payments --to build-02 --stop-runs --wait
spaceman space relocate status payments
spaceman space relocate retry payments --dirty-state export --wait   # after a blocked check
spaceman space relocate release payments --yes                     # delete the retained source folder
```

When the source node is offline, disabled or revoked, the unavailable-source flow relocates from central's copies only.
It needs the explicit acknowledgement that local state may be lost, in your own words (recorded in the audit log), and
the repositories to clone on the destination:

```sh
spaceman space relocate start payments --to build-02 --source-unavailable --acknowledge-loss \
  --acknowledgement "build-01 disk failed; unpushed work there is lost" \
  --repo https://git.example.com/payments.git@main
```

`--wait` polls until the relocation is done, checked (precheck), blocked or failed, and prints the issues found. See
[Relocating a space](./relocation) for what each step does.

## Conventions

- **Creates** send a fresh `Idempotency-Key` where the operation accepts one, and are retried on connection failures and
  502/503/504 with the same key, so a lost response never creates a second object. Pass `--idempotency-key` to make a
  scripted create safe to re-run. Other creates are never retried.
- **Updates** read the object first and send its version as `If-Match`; only the flags you give change, the rest is sent as
  read. With `--if-match N` the update is based on a version you read earlier, so a change made in between is a conflict
  (exit code 3) instead of being overwritten.
- **Bodies** can come from a JSON or YAML file with `-f FILE` (`-` reads standard input); flags override its fields. Long
  text fields have a `--…-file` variant.
- **Secrets** (account secrets, Pulse credentials, MCP request headers, OIDC client secrets, passwords, TOTP codes) are
  read with `--…-file PATH` or `--…-stdin`, never from flag values, and are never printed. One-time secrets that the
  server returns are printed once, or written to a new `0600` file with `--secret-file`. Enrollment and API tokens
  (`node create`, `node enrollment`, `token create`) are printed unless `--secret-file` is given; every other one-time
  secret (`user reset-token`, `user link-invite`, `mfa enroll`, `mfa confirm`, `mfa recovery-codes`,
  `system root-token rotate`) is shown only on request: the command refuses to run without `--secret-file` or
  `--show-secret`.
- **Downloads** (`attachment download`, `run recording download`, `run transcript download`, `memory file --out`)
  write to `--out FILE`: a new `0600` file that appears only once it is complete, never replacing an existing file
  without `--force`. `--out -` writes to standard output only when it is a pipe or a file, never a terminal: content may
  be binary, carry escape sequences or contain secrets. `memory file` without `--out` prints the agent-written text
  with control characters removed.
- **Lists** that the API pages (inbox, audit) follow `next_cursor` with `--all`; without it the CLI prints one page and
  the cursor to continue from.
- **Irreversible operations** need `--yes`: `node revoke`, `space purge`, `space relocate release`,
  `run recording delete`, `run transcript delete`, `prompt discard-draft`, `event subscription quarantine discard`,
  `oidc provider delete`, `identity unlink`, `user identity unlink`, `mfa recovery-codes` and
  `system root-token rotate|revoke`.
- **Updates of resources without a single-item read** (dream configurations, human-action policies) read the list,
  change only the flags given and send the version read as `If-Match`. `task set-policy` reads the queue policy and its
  version just before the change.
- **The change feed**: `spaceman watch` follows `GET /api/v1/watch` until interrupted (or `--count` changes), one line
  per change (JSON Lines with `-o json`); a gap after `--after` is reported on standard error.
- **Errors** are the API's RFC 9457 problems: the message, the stable code, field errors, the operation and the request
  ID (quote it when reporting a problem).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | usage: bad flags or arguments, unreadable input files, local failures |
| `2` | API error, a final state you did not want (failed or blocked relocation with `--wait`, broken audit chain), or the server could not be reached |
| `3` | conflict: `409`, `412` or `428` (the object changed since it was read, already exists, or a precondition failed) |
| `4` | not found: `404` (a space you cannot read looks the same) |
| `5` | authentication or authorization: `401`, `403`, or no usable token |

## Operations without a command

Every operation of the [HTTP API](/reference/api) has a command except these, which belong to the browser's sign-in
flows. A test fails when a new operation has neither a command nor an entry here (ADR-0040).

| Operation | Why |
|---|---|
| `login`, `verify-mfa` | interactive sign-in: it creates a browser session (cookie and CSRF token); the CLI authenticates with an API token |
| `logout` | ends the browser session it is called from; revoke API tokens with `spaceman token revoke` |
| `reauth-password`, `reauth-totp` | step-up of a browser session; API tokens are not subject to re-authentication |
| `recovery-login` | break-glass sign-in that creates a browser session; use the recovery token on the sign-in page |
| `start-oidc`, `oidc-callback` | browser redirects to and from the identity provider (sign-in, step-up, link, test login) |
| `reset-password` | anonymous: central accepts it only from its own web origin, so the user redeems the token from `spaceman user reset-token` on the sign-in page |

The live terminal stream (`…/runs/{run_id}/terminal`) has no command either: it replays a program's terminal output
with its escape sequences, which belongs in the web UI's terminal viewer. `spaceman run recording download` saves the
recording to a file instead.

## Backups

`spaceman system backup list|create|verify` asks a running central (the **Backups** page's operations). The offline
`spaceman backup` commands read the data directory with the KEK, and restoring stays offline
(`spaceman backup restore`, see [Backups and restore](./backup)). `spaceman artifacts migrate` moves stored artifacts
between the local disk and an S3 bucket while central is stopped ([Artifact storage](./storage)).
