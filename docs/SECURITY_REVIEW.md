# SpaceMan — Security review (M4-08)

> Requirement: SM-SEC-001 (documented boundaries), with the threat model in [THREAT_MODEL.md](THREAT_MODEL.md).
> Behaviour changes from this review are recorded in ADR-0039 ([DECISIONS.md](DECISIONS.md)).

## Summary

| | |
|---|---|
| Date | 2026-11-03 |
| Revision | `dd37bb6` plus the changes of the M4-08 commit |
| Scope | Central (`internal/api`, `internal/auth`, `internal/authz`, every feature service), the node agent (`internal/agent`), the agent tool gateway and run credentials, the MCP gateway, OIDC, Pulse, S3, backups, relocation, memory, KB, skills, attachments, recordings and transcripts. Not in scope: the web UI's own code beyond its use of the API, the Terraform provider, the CLI, and third-party dependencies (covered by `govulncheck` and the SBOM). |
| Method | An adversarial manual reading of the code, split into four areas: (1) authorization and run-token scope, (2) path traversal and SSRF/egress, (3) secrets, CSRF, origin, If-Match, idempotency, rate limits and prompt-injection surfaces, (4) a fault-injection pass (the `make test-fault` suite). Each area traced the code path end to end from the entry point (HTTP route, node message, agent tool, Git invocation) to storage. Three of the path findings were reproduced with throwaway Git repositories before they were fixed. Each fix has a test that fails without it. |
| Result | No high-severity findings. Four medium findings, all fixed. Fourteen low findings: eight fixed, six open or accepted with a rationale. Eight informational notes. |

Severity: **high** means a remote or cross-space compromise without prior access. **Medium** means an escalation within the trust model: agent code running as the node daemon, or a weakened authentication factor. **Low** means a correctness or defence-in-depth gap with limited impact. **Info** means a design note.

## Findings

| ID | Severity | Finding | Status | Covered by |
|---|---|---|---|---|
| SR-M01 | medium | **Login and MFA throttles could be raced.** The throttle check, the password or TOTP verification and the recorded failure were separate steps. A burst of parallel attempts all read "not throttled" before any failure committed, so a password holder could try many TOTP codes per throttle window. The per-IP limiter skips authenticated (Limited) sessions. | fixed in this change: attempts are serialized per throttle key (account, MFA factor) in central (`internal/auth/attempts.go`) | `internal/auth` `TestMFAThrottleHoldsUnderConcurrency`, `TestLoginThrottleHoldsUnderConcurrency` |
| SR-M02 | medium | **A relocation could create a Git worktree outside the space folder on the destination node.** Worktree paths from the source's inspection report were neither validated centrally nor on the destination. A worktree path through a symlink committed in the repository, or a `../` path from a compromised source, wrote agent-chosen files anywhere the destination's OS user can write. | fixed in this change: central refuses report paths outside the space folder. The destination requires a relative path outside `instances/` and `dreams/`, refuses an existing folder, and checks for symlinked parents before and after creating them. | `internal/agent` `TestAddWorktreesRefusesEscapes` |
| SR-M03 | medium | **Writer worktree preparation ran the repository's hooks.** `prepareWorktree` ran `git worktree add` with neither hooks nor fsmonitor disabled. A `post-checkout` hook planted by an agent ran as the node daemon, outside the harness's sandbox. KB worktrees use the same path. | fixed in this change: `core.hooksPath=/dev/null`, `core.fsmonitor=false` and a scrubbed Git environment, as for relocation | `internal/agent` `TestPrepareWorktreeRunsNoRepositoryCode` |
| SR-M04 | medium | **Repository configuration could run programs despite the "safe" Git options.** Clean and smudge filters, merge and diff drivers, pagers, SSH and proxy commands, credential helpers, includes, URL rewriting and protocol allowances in a repository's `.git/config` still ran during the daemon's `add -A` (memory checkpoints, relocation snapshots), `worktree add` (KB) and `merge` (memory, KB). | fixed in this change: before Git runs in an existing repository, the daemon reads the repository's own (local, worktree) configuration and refuses keys that would run a program. This applies to memory, KB, writer worktrees and relocation. | `internal/memory` `TestUnsafeConfig`, `TestRunRefusesUnsafeRepositoryConfig`; `internal/agent` `TestPrepareWorktreeRunsNoRepositoryCode` |
| SR-L01 | low | **Idempotency was not atomic.** The stored result was read, the operation ran, and the result was then stored. Two concurrent retries with one `Idempotency-Key` created two resources. | fixed in this change: requests with the same principal, operation and key run one at a time | `internal/app` `TestConcurrentIdempotentCreatesOnce` |
| SR-L02 | low | **Purging a space left its grants in the authorization engine.** `fleet.PurgeSpace` deleted `role_grants` with SQL and never reloaded Casbin, so the grants stayed effective in memory until an unrelated reload. Known issue from SM-AUTH-007. Low because space IDs are never reused. | fixed in this change: purge removes the grants through the auth service, which reloads the engine and re-checks open streams after commit | `internal/app` `TestPurgeSpaceForgetsItsGrants` |
| SR-L03 | low | **User `If-Match` was checked outside the write transaction.** Two administrators changing one user's grants or enabled state at once could both pass the version check, and the last write wins. | open: needs the version passed into `auth.SetGrants`/`SetEnabled`. Concurrent administrators are the only actors. | `TestAPIBootstrapLoginAndConventions` (sequential only) |
| SR-L04 | low | **Loose-file restore could be escaped with chained symlinks.** Symlinks were created in a second pass without re-checking, so `a -> /x` followed by `a/b` created `/x/b`. A symlink named `instances` was also accepted. | fixed in this change: each symlink is re-checked for symlinked parents when it is created, and `instances` is refused whatever its type. The relocation walk skips `instances` whatever its type. | `internal/agent` `TestReadLooseRefusesChainedAndReservedLinks`, `TestReadLooseRefusesUnsafePaths` |
| SR-L05 | low | **Any run in a space could reply to (and so resolve) any request it knew the ID of.** `spaceman_message_reply` skipped the recipient check that `spaceman_message_resolve` has, so a worker could close control's request to the operators. | fixed in this change: only a recipient may reply | `internal/messages` `TestTools` |
| SR-L06 | low | **Deleting a transcript needed no step-up**, although deleting a recording does. | fixed in this change: same rule as `delete-recording` (re-authentication with MFA) | `internal/app` `TestReviewDeleteStepUpAndQueueRecord` |
| SR-L07 | low | **Viewers could write through `GET …/queue/next?record=true`** (a stored decision under `queue:read`). | fixed in this change: `record` needs `queue:write` | `internal/app` `TestReviewDeleteStepUpAndQueueRecord` |
| SR-L08 | low | **Event-template instructions could carry event text into trusted metadata.** `instructions` were rendered with the payload, so `{{data.issue.body}}` put a third party's text under `trusted.instructions`. The seeded templates did not do this. | fixed in this change: validation allows only `{{vars.*}}` in instructions, and evaluation renders them with the variables alone, also for stored versions | `internal/events/template` `TestValidateDefinition`, `TestInstructionsSeeOnlyVariables` |
| SR-L09 | low | **Reset-token redemption has no throttle and no failure audit**, unlike recovery login. It also hashes the new password before checking the token. | open: tokens are 256-bit, so guessing is infeasible. The in-memory per-IP limiter bounds the Argon2 cost. Follow-up: use the recovery-login pattern. | — |
| SR-L10 | low | **The enrollment limiter keys on the socket address**, not the trusted-proxy client IP. It does not group IPv6 by /64, and failed enrollments are not audited. Behind a reverse proxy every node shares one bucket. | open: enrollment tokens are single-use and 256-bit. Follow-up: reuse the API's client-IP logic and audit failures. | — |
| SR-L11 | low | **Untrusted-content fences can be spoofed.** Cross-space request bodies are fenced with fixed `---` lines, and task fields go into worker prompts unmarked (delegated tasks come from other spaces). | open: the text is still never an authority source, because tools are authorized centrally (SM-INV-018). Follow-up: a random fence per message and labelled task fields. | `TestCrossSpaceRequests` (marking present) |
| SR-L12 | low | **Free text in readable places.** A relocation's loss acknowledgement is sealed in the audit log but readable by space viewers on the relocation record. Some `reason` fields are written to plaintext audit details. | accepted: this is operator-written text about the operation, not a secret. Sealing it in the audit log keeps it out of audit exports. Reasons written by agents are bounded and never authority. | — |
| SR-L13 | low | **The destination clones the remote URL the source reported.** An agent can point `origin` at an internal address or a local path on the destination. | open: the destination fetches with its own Git defaults (`ext::` disabled). Follow-up: allow only `https`, `ssh` and scp-style remotes from reports, keeping local paths for operator-supplied specs. | — |
| SR-L14 | low | **Some idempotency fingerprints cover only part of the request.** Accounts (`name`, `kind`, the secret deliberately left out), attachments (`space`, `name`, `size`: different bytes of the same size replay the first upload), MCP servers and OIDC providers. | open: none crosses a principal, because keys are per principal. Follow-up: include a content digest for uploads. | — |
| SR-I01 | info | OIDC and S3 private-address checks are narrower than the MCP gateway's: no NAT64, 6to4, 198.18/15 or 240/4, and link-local is allowed with `allow_private_network`. Both are configured by administrators. | open | `internal/oidc`, `internal/s3client` `TestPrivateNetworkRefused` |
| SR-I02 | info | The go-pulse SDK follows the `connect_urls` a NATS server advertises (no `IgnoreDiscoveredServers`). The Pulse URL is set by an administrator. | open | — |
| SR-I03 | info | `list-event-connections` needs only a session, and the `connection:read` permission is never checked. A space operator may subscribe to, and replay, any connection. Connections carry no secrets in reads. | open: needs a decision on connection scoping | `TestEventsOverHTTP` |
| SR-I04 | info | `run:diagnose` and `terminal:takeover` are declared but not enforced: operators launch diagnostic runs with `run:control`, and `send-input` with `force` types into a busy terminal. SM-UI-004 records takeover as pending. | open | — |
| SR-I05 | info | Some cross-references are stored without a space check: a blocker's `ref_id`, an inbox `linked_id`, a task's `assignee_run`, a launch `instance_id` and a KB proposal's `request_id`. They grant no access. | open | — |
| SR-I06 | info | `cancel-relocation` has no step-up, unlike the other relocation operations. Cancelling is the safe direction. | accepted | `TestRelocationOverHTTP` |
| SR-I07 | info | Minor API conventions: the API-token failure limiter runs after the token check (irrelevant with 256-bit tokens), `If-Match` accepts weak validators, a stale version is 409 rather than 412, and a credential placed in an MCP server URL's query string would be shown in reads. | accepted | `TestAPIBootstrapLoginAndConventions` |
| SR-I08 | info | Found by the fault suite: a stopped in-process agent left its recorders attached, so the next agent's recorder competed for the run's output. In a real process exit the recorders die with the process. | fixed in this change: `Agent.Run` ends its recorders when it returns | `test/fault` `TestFaultInterruptedRecordingAndUpload` |

## Reviewed and found correct

- **Route authorization:**
  - Every `/spaces/{space_id}` operation takes its Casbin domain from the path. A space the caller cannot read answers `not_found`.
  - Every sub-resource (run, task, inbox item, attachment, human action, approval, message, memory conflict, KB request, dream, schedule, subscription, delivery, quarantine entry, replay, cross-space request) is loaded with its space ID in the service.
  - Global lists (spaces, human actions, change feed) filter by the caller's grants per space.
  - Tests: `TestEveryOperationHasARule`, `TestWatchStreamFiltersAndExpires`, `TestPermissionMatrix`.
- **Run tokens:**
  - The hub authenticates the token, matches the run and the run's node, and refuses ended or revoked runs.
  - Every built-in tool takes the space and run from the authenticated run, never from its arguments.
  - The registry enforces the run kind: control-only tools, KB sessions without orchestration, the dreamer's report bound to its run.
  - The MCP gateway rechecks the grant on every call.
  - Tests: `TestAgentToolsOverBridge`, `TestOnlyControlAsksOtherSpaces`, `TestHumanResponsesReachOnlyTheirTarget`, `TestUploadSniffDedupAndScope`.
- **Egress:**
  - The MCP gateway checks the resolved address at dial time (DNS rebinding included), with no proxies, redirects refused and bounded bodies (`TestEgressPolicy`).
  - OIDC discovery, token and JWKS fetches all use one guarded client with redirects refused.
  - S3 uses the same dial-time hook (`TestPrivateNetworkRefused`).
- **Paths:**
  - Attachments are content-addressed behind a strict digest check.
  - Backup restore validates every path with `filepath.IsLocal` and refuses symlinks (`TestVerifyDetectsTampering`).
  - Relocation blobs are single path elements. Skills are validated centrally and on the node, written 0600 and never executable (`TestSyncSkills`).
  - Run, space and recording IDs are format-checked before they become paths.
- **Browser sessions:**
  - Every mutating request without a bearer token needs a matching `Origin`, or `Sec-Fetch-Site: same-origin`, and an HMAC CSRF token. It fails closed when both headers are missing.
  - Cookies are `__Host-`, `Secure`, `HttpOnly` and `SameSite=Lax`. No CORS headers are sent (`TestAPIBootstrapLoginAndConventions`).
- **Secrets:**
  - Write-only everywhere. Run secrets are attached only when the launch is sent, and the node journal stores no payload (`TestLaunchPayloadRedacted`).
  - Validation problems drop submitted values, URLs with userinfo are refused, and log keys are redacted.
  - The outbox catalog is metadata-only, and the change feed carries IDs only.
  - Audit outcomes are only `success`, `failure` and `denied`.
- **Throttles** are durable and keyed per account and per IP for login, and per IP for recovery login. They fail closed on database errors (`TestLoginThrottlingAndEnumeration`, `TestIPThrottle`).
- **Prompt-injection surfaces:**
  - Notices typed into runs carry counts only.
  - Inbox trusted metadata comes from the authenticated source and the template's variables.
  - No agent tool can decide an approval.
  - Cross-space permissions come from the stored peer grants, never from a payload.

## Fault-injection results

`make test-fault` (`test/fault`, simulated, in-process central and agent with the in-memory runner). Each test is listed in [TEST_STRATEGY.md](TEST_STRATEGY.md). Found and fixed:

- A full disk silently ended central's copy of a recording. The node now ships a `trunc` record (`disk`) and the run's close record, and a quota truncation is shipped too.
- Writers that lost records to a failed disk could be renumbered under central's copy (`recording.Writer.SkipTo`).
- Recorders outlived a stopped agent (SR-I08).
- A storage-busy API error had no `Retry-After`. It now sends `Retry-After: 1`, and nothing is written, so a retry with the same `Idempotency-Key` creates the resource once.
