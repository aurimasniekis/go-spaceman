# SpaceMan — Security review (M4-08)

> Requirement: SM-SEC-001 (documented boundaries), with the threat model in [THREAT_MODEL.md](THREAT_MODEL.md).
> Behaviour changes from this review are recorded in ADR-0039 ([DECISIONS.md](DECISIONS.md)).
> A later entry covers the SpaceMan admin MCP (ADR-0063): [SpaceMan admin MCP](#spaceman-admin-mcp-adr-0063-2026-11-08).

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

## SpaceMan admin MCP (ADR-0063, 2026-11-08)

A review entry for the SpaceMan admin MCP (SM-MCP-005..009, AM-1 to AM-10), written with AM-8 at the end of the work
stream on branch `feat/spaceman-mcp`. It records how the change sits against the threat model, which rails were checked
and by which tests, and the operator's decisions on the open question and the accepted risks. It is not a repeat of the M4-08 review method above: each rail was traced
from the gateway or endpoint entry point to the API's handler chain and checked against its tests. Labels: **simulated**
evidence only (in-process central, real agent and bridge, fake runner; the go MCP SDK client over HTTP). No real
harness has driven the tools through a model yet.

| | |
|---|---|
| Threats | **T-33** (an agent in an infra space uses its owner's administrator authority through the tools): accepted risk, operator decision 2026-11-07, for a trusted infra space. **T-38** (an operator's own MCP client on `/api/v1/mcp`, steered by prompt injection or a web page): mitigated as below, with accepted residuals. See [THREAT_MODEL.md](THREAT_MODEL.md). |
| Result | No finding to fix. SR-MCP-01 (root approval) resolved by the operator on 2026-11-08: kept. The final review's F1, F2 and F6 are accepted risks (operator decision 2026-11-08). ADR-0063 is **Accepted** (2026-11-08). |

### Rails checked

- **Identity (SM-MCP-006).** A SpaceMan server's internal token has no secret and is never accepted as a bearer token
  (`AuthenticateAPIToken` refuses any purpose). Each call builds the principal from the token ID and is authorized as
  token **and** owner, so it loses what its owner loses. Revocation, owner disable, grant loss, expiry, server delete
  and disable all stop it on the next call, with the reason. Tests: `TestInternalTokens`, `TestSpacemanServers`,
  `TestSpacemanMCPGateway`, `TestSpacemanMCPOverBridge` (token revoked mid-run: the next call through the bridge is
  refused with "revoked … re-issue", the catalog empties and the run goes on).
- **Hard exclusions (SM-MCP-007).** Every operation has a reviewed class (`TestEveryOperationIsClassified`). Sign-in,
  step-up, tokens, users, approvals and approval policies, human-action answers, takeover and terminal input, one-time
  secrets, sign-in providers, SpaceMan MCP management and restore acknowledgement are never tools
  (`TestHardExclusions`). The API itself also refuses internal and endpoint principals on Session (unless filtered),
  UserOnly, Reauth and MFA rules, although bearer tokens otherwise skip step-up
  (`TestInternalPrincipalRefusedForPersonalOperations`, `TestMCPEndpointGuard`).
- **Reach.** Only control and worker runs get the tools; KB, dreamer and diagnostic runs never do. A role the server
  was not granted to sees no tools and is refused (`TestSpacemanMCPGateway`, `TestSpacemanMCPOverBridge`).
- **Approvals model (SM-MCP-008).** Dangerous tools always need a person's approval, whatever the preset or the
  server's `approval` setting. Without a stored policy, `approvals.Require` uses a synthetic one-use policy bound to the
  server and the exact arguments (the request records the run); a stored `mcp.*` policy written by a person replaces it.
  `approval: writes` gates writes too; the default `none` lets writes run at once (operator decision, the T-33
  residual). An agent is never an approver: the approver re-check refuses any `spaceman_mcp:` actor or internal token,
  and human-action responses refuse it at the service level (`TestApproverRecheckRefusesSpacemanMCP`, `TestRequire`).
  End to end over the bridge, an approved dangerous call runs exactly once (one execution, no uses left) and a replay
  asks again (`TestSpacemanMCPOverBridge`, `TestSpacemanMCPGateway`). Fail closed: without an approval gate a dangerous
  call is refused.
- **Resolved-kind diagnostic gate.** The gateway classes a call by its arguments, but `launch-run` may take its kind
  from the space role's template, which only the API resolves. The API refuses a diagnostic launch by an internal or
  endpoint principal unless the call carries the gateway's mark (`gateway.WithApprovedCall("launch-run")`), set only
  after an approval succeeded for that exact dangerous call. A role-only diagnostic launch is refused with a hint to pass
  `kind: diagnostic`. Saving a space role is dangerous, so a role can't be turned diagnostic without an approval either
  (`TestAuthorizeLaunchKind`, `TestSpacemanMCPGateway`).
- **External endpoint (SM-MCP-009, T-38).** Only a user's bearer API token: session cookies and the root token get
  403, internal tokens 401. Browser requests (`Origin` or `Sec-Fetch-*`) are refused and no CORS headers are sent.
  Per-token rate limit 5/s, burst 50, plus the API's body and failed-bearer limits. Every call is audited via the token
  on the `mcp_endpoint` channel, without arguments or results (`TestMCPEndpoint`, `TestMCPEndpointGuard`).
- **Batch refusal.** The per-token limit counts requests, so a request must carry one call: a JSON-RPC batch (a body
  whose first non-space byte is `[`) is refused with 400 before any message in it runs, and so is an
  `Mcp-Protocol-Version` older than 2025-06-18, the version that dropped batching (the SDK would accept batches for
  older versions). `TestMCPEndpoint` sends a batch of 20 calls and checks that none ran.
- **The token's user may deny, never approve.** On the endpoint a dangerous call's approval is bound to the token
  (`api_token:<id>`) and the exact arguments. The token acts for its user, so that user approving would add no second
  check (a stolen token plus that user's session would approve itself): `onFinalized` refuses an approving decision by
  the token's user in any session or through any of their tokens, and the gate revokes such an approval if one is
  recorded anyway. The user may still deny or withdraw their own client's pending request from a session or the API,
  never through the MCP channel. Consequence: a single-administrator installation can't approve the endpoint's
  dangerous calls with a user's decision; there the root token approves them (SR-MCP-01, kept) (`TestMCPEndpoint`).
- **Audit.** In-run calls are attributed to the owner with `via spaceman_mcp:<token>` and `run`; the gateway's
  `mcp_server.tool_called` names the run, the server, the operation and the class. The audit chain verifies after the
  end-to-end run (`TestActorViaAndRun`, `TestSpacemanMCPOverBridge`).

### Decisions and accepted risks

| ID | Severity | Item | Status |
|---|---|---|---|
| SR-MCP-01 | info | **May the root token approve the external endpoint's dangerous calls?** The "token's user never approves" rule matches user decisions only, so a decision made with the root token is accepted. On a single-administrator installation it is the only way to approve those calls; refusing it would also mean those calls can't run there at all. | **Resolved: kept** (operator decision 2026-11-08). The root token may approve them; it is the documented path for single-administrator installations. The root token itself is refused on the endpoint, so it can't make the call it approves (ADR-0063 §External endpoint, T-38). |
| F1 | info | **Who approves in-run dangerous calls?** An in-run call's approval is requested in the run's own space, so any operator there with `approval:decide` may approve calls of the infra space's SpaceMan MCP server that act on other spaces or on installation-wide targets (`admin@*`). | **Accepted risk** (operator decision 2026-11-08). Deliberate for the trusted infra space, whose operators are trusted with the server's reach; part of T-33's accepted risk (ADR-0063 decision 7). |
| F2 | info | **`launch-run` is a write, also with overrides.** A launch may override `settings.permission_mode` (held to the installation's `harness.max_permission_mode` ceiling) or `account_ref` without an approval under the default `approval: none`, while `save-space-role` is dangerous. | **Accepted risk** (operator decision 2026-11-08), a residual under T-33. A launch's override affects only the run it starts, which the call names and the audit records; a role save persists and reaches every later launch by that role, so only the save is dangerous. Diagnostic launches stay gated on their resolved kind (ADR-0063 decision 7). |
| F6 | info | **`/api/v1/mcp` is on by default.** Every user API token can use the endpoint within its grants; there is no setting to turn it off. | **Accepted risk** (operator decision 2026-11-08). The endpoint adds no authority beyond what the token has on the API; an operator who doesn't want it keeps the path out of the reverse proxy (ADR-0063 §External endpoint, T-38). |
