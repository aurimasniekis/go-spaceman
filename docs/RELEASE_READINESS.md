# Release readiness report

> Work package M5-05 (GOAL §8). Written 2026-11-03 at the head of `main` after the commits listed below.
> This report makes **no production-readiness claim**. SpaceMan is feature-complete against the brief with the gaps
> recorded here and in [LIMITATIONS.md](LIMITATIONS.md). It has not been released, published or run by anyone but its
> developer, and several dependencies have only simulated evidence.
>
> **v0.1.0 final (2026-11-08):** §14 records the final release: what it contains since rc.9, its migrations, node
> protocol and tested Claude Code version, the known issues it ships with and the accepted risks.
>
> **v0.1.1 (2026-11-09):** §15 records the patch release: what it contains since v0.1.0, its migrations, node
> protocol, upgrade notes and the known issues it ships with.
>
> **v0.1.2 (2026-11-13):** §16 records the patch release: what it contains since v0.1.1, its migrations, node
> protocol, upgrade notes and the known issues it ships with.

## 1. Verdict

| Question | Answer |
|---|---|
| Is every requirement implemented? | 218 of 220 requirements are `implemented`, 1 is `partial` with a stated gap, 1 is `n/a` ([TRACEABILITY.md](TRACEABILITY.md)). None is left `todo`. |
| Is it tested? | Yes. Every gate below passes on the final tree, including the race-enabled suite, the fault-injection suite, the browser journey in three engines, the Terraform acceptance tests and the scripted 30-step demonstration. |
| Is it verified against real dependencies? | Partly. herdr-core, Claude Code, Porch, Pulse with go-pulse-github, an S3-compatible store (MinIO), Linux with systemd (in a container), container images and three browsers have real evidence. Codex, AWS S3, a physical Linux host, amd64 under systemd and real Google or GitHub accounts do not. |
| Is it ready for production? | **No claim is made.** It is ready for evaluation by a trusted team on macOS or Linux, with the limitations below. Before a production release: run it on a physical Linux host, verify Codex, run the release workflow, publish the site, and close the open security findings. |
| Is it released? | No. No tag, no published image, no published site. The release workflow only drafts a GitHub release and has never run. |

## 2. Implemented features

Grouped by milestone; the per-feature detail is in the site's guides and in the progress log of [PLAN.md](PLAN.md).

- **Foundation (M1).** Central (`spaceman`) as a modular monolith on SQLite (WAL, split pools, goose migrations, sqlc),
  keyring with a KEK file, hash-chained audit, huma OpenAPI 3.1 API with RFC 9457 errors, If-Match, idempotency,
  cursor pagination and a durable change feed. Root-token bootstrap, Argon2id passwords, TOTP, recovery codes, sessions,
  CSRF, throttling, scoped API tokens, Casbin RBAC with space domains. The node agent (`spaceman-agent`): enrollment,
  outbound WebSocket protocol with execution leases and fencing, a node journal, herdr-core runner with idempotent
  launch and adoption, a per-space lease guard, Claude Code and Codex adapters with isolated config directories and a
  secret-delivering launch shim, an MCP bridge, recordings in zstd segments with live view and replay. Web UI (React,
  Mantine 8, Night Owl, TanStack Router/Query/Form, ghostty-web terminal).
- **Coordination (M2).** Attachments and the TipTap Markdown composer; the space inbox; Pulse ingestion with one durable
  per subscription, dedup, quarantine and fan-out; event templates with GitHub presets; ingestion metrics and
  non-durable core NATS subscriptions; the execution queue with dependencies, blockers, exclusions, incidents and
  authoritative manual ordering; control and worker runs in separate worktrees; durable messages; human actions.
- **Configuration and intelligence (M3).** Accounts with sealed write-only credentials; a versioned prompt catalog,
  harness profiles and role templates with resolved manifests; skills; the MCP gateway with grants, namespacing, limits
  and an egress policy; custom human-action types with JSON Schema 2020-12 forms; enforced approvals bound to an
  argument digest; role memory as Git with central sync; knowledge bases with per-requester support sessions; the
  dreamer; usage from harness records and stored transcripts.
- **Hardening (M4).** OIDC sign-in (Porch contract), linking and step-up; revocation of open streams; quotas, limits,
  budgets, schedules and explicit replay; outbound events through a transactional outbox; sealed backups and a restore
  that comes up paused with new execution authority; explicit, fenced space relocation; cross-space requests with
  two-sided grants; human-action reminders, escalation and response routing; a fault-injection suite and a security
  review; packaging (images, systemd units, release builds with SBOMs and checksums).
- **Completion (M5).** A Terraform provider (30 resources, 7 data sources), in its own repository
  `terraform-provider-spaceman` (ADR-0047); a remote CLI covering 239 of 248 API
  operations; the documentation site with architecture, operator, reference and development pages; the scripted §41
  demonstration; real-dependency suites.

## 3. Architecture decisions

59 ADRs in [DECISIONS.md](DECISIONS.md). The ones that shape operation most:

| ADR | Decision |
|---|---|
| 0001 | Modular monolith, single active central, cold standby (no HA). |
| 0006, 0007, 0017, 0043 | Outbound node connection with execution leases and fencing; herdr-core runs programs; a per-space lease guard ends runs when the agent dies, and lives only while the space has runs. |
| 0008, 0009, 0012, 0022 | Per-space harness config directories; secrets through a launch shim; run-scoped credentials and tools over the node connection. |
| 0019, 0029, 0033 | Root-token bootstrap and scoped tokens; OIDC with SSO grants only for SSO sessions; revocation reaches open streams at once. |
| 0023, 0024 | MCP gateway with per-run upstream sessions and an egress policy; enforced approvals bound to an argument digest. |
| 0025, 0026, 0027 | Role memory as Git owned by the node and preserved centrally; KB support sessions; dreams. |
| 0030, 0042 | Outbound events via a transactional outbox; core NATS as a labelled non-durable delivery mode. |
| 0031, 0037 | Sealed backups with a paused restore; explicit, resumable, fenced relocation. |
| 0034 | What the container images contain, and the node agent's looser sandbox. |
| 0035, 0040, 0041, 0047 | Terraform and CLI as clients of the same API; secrets never as flag values; Terraform owns only what it configures; the provider lives in its own repository. |
| 0038 | Cross-space requests: two-sided grants, destination-owned execution, disclosure applied to replies. |
| 0039 | Security review fixes, including Git in the node daemon running no repository-supplied programs. |
| 0044 | Configured space repositories: a central list, cloned, fetched and reported by the node with its own Git authentication; no Git credentials stored. |
| 0045 | Retention as limits with one daily pass, plaintext space exports, a resumable audit log, memory flushed before backups, and Hallpass sign-in parity (SSO-only, direct provider, provider sign-out). |
| 0046 | UI gap closure: terminal takeover through herdr-core's interactive attach (permission, recent sign-in, no automated input while held), clarification notices, the attention summary and per-user notification preferences. |
| 0048 | Artifacts on local disk or in an S3 bucket behind one store interface; backups still copy S3 artifacts; interfaces for inbound event sources and MCP transports. |
| 0049 | Paused-space event policies (`while_paused`: defer, wake, stop), automation depth carried between installations and bounded by `event_depth`, and responses as metadata-only inbox items (`response_inbox`). |
| 0050 | Trusted incident triggers with proposal or refusal for the rest, handling and resumption of preempted work, `run:diagnose`, per-type human-action permissions and user profile edits. |
| 0051 | Starter roles: example profiles, prompts and role templates added once, given to spaces on request, editable and deletable. |
| 0052 | Notices to programs that finished their turn, a 160×48 terminal size, a web terminal that survives reopening, one change-feed stream per browser. |
| 0053 | Codex ChatGPT logins as one live file per account on the node, shared by its instances; Codex skills in `$CODEX_HOME/skills`. |
| 0054 | An OpenAI API key reaches Codex as a stored login in its `CODEX_HOME`, since interactive Codex ignores key variables. |
| 0055 | Release images published to Docker Hub (`aurimasniekis/spaceman`, `aurimasniekis/spaceman-agent`) for every release tag. |
| 0056 | Control starts for waiting work when no control run is live, and stops after an idle timeout (two space limits). |
| 0057 | Repository SSH keys held by SpaceMan: sealed in central, sent only with a clone or fetch, served from an in-memory agent on the node against pinned host keys; agents never receive them. |
| 0058 | Shared Git SSH keys: one administrator-managed key on a Git account, scoped to listed spaces, that repositories choose instead of their own deploy key. |
| 0059 | Claude Code settings on harness profiles: the profile's settings.json keys (permissions, auto mode, hooks, any key) set in each run's settings.json, defaultMode under the ceiling, protocol minor 8. |
| 0060 | Harbor Pilot gateway (Accepted 2026-11-08): one central connection with sealed write-only tokens, spaces opt in per harness, a child token per run minted at dispatch and revoked on every end path, no fallback to accounts, Harbor Pilot budgets as an early refusal, protocol minor 9. See §13. |

## 4. Commits

All work is on `main` as Conventional Commits created through `/cc`, except the three recorded process deviations
(D-1 the root commit, D-2 `914b461`, D-3 `2e37fee`: messages written by hand; [PLAN.md](PLAN.md) §4). Nothing has been
pushed. The progress log in [PLAN.md](PLAN.md) maps each work package to its commits; `git log --oneline` lists them all.

## 5. Tests executed and results

All on 2026-11-03, macOS 15 (Darwin 24.6, arm64), Go 1.27.3. "Final tree" means the head of `main` after the
last code change (`5381ffb`); the other rows ran earlier the same day, on the commit named.

| Gate | What it runs | Where | Result |
|---|---|---|---|
| `make check` | vet, the custom `txscope` analyzer, golangci-lint, the race-enabled test suite, analyzer tests, coverage floors (≥ 80% for every listed package), traceability check | final tree | pass |
| `make web-check` | typecheck, vitest, production build, dist check | `4445a03` | pass |
| `make e2e` | the browser journey against a real central, agent and isolated herdr-core core | `4445a03` | pass |
| `make e2e-browsers` | the same journey in Firefox and WebKit | `261553a` | pass |
| `make screenshots` | every documentation screenshot in both themes, with an axe WCAG 2.1 AA audit | `8c383c0` | pass |
| `make site-check` | site build with dead-link check, screenshot inventory, Mermaid, the site's axe audit, API/CLI/config reference drift | final tree | pass |
| `make test-fault` | disk full, interrupted recording and upload, killed agent, central killed mid-write, disconnect past lease and grace, partition during a command, corrupt node state, busy database | `4445a03` | pass |
| `make test-pulse` | ingestion and outbox against an embedded NATS server with the real go-pulse SDK | `c87ba00` | pass |
| provider `make check`, `make acc` | in the provider repository: unit tests, lint, and the acceptance tests (Terraform 1.17) against a central built from this checkout | provider repository against `35fa6a6` | pass |
| `make demo` | the brief's 30-step demonstration (31 with setup), stub harness | `4445a03` | 31/31 pass |
| `SPACEMAN_DEMO_REAL=1 make demo` | the same with a real Claude Code worker | `3baa1d9` | 31/31 pass |
| real suites | `make test-real`, `test-real-porch`, `test-real-pulse`, `test-real-s3`, `test-real-systemd` | `261553a`; node suites again at `4122671` | pass (opt-in; see §6) |

Known test fragility: a few timing-sensitive tests (the login-timing check and some restart and routing tests) have
failed once each under extreme machine load (load average above 100, several suites in parallel) and pass on rerun; see
[PLAN.md](PLAN.md), Open issues.

## 6. Real versus simulated evidence

| Dependency | Real evidence | Not verified |
|---|---|---|
| herdr-core 0.1.16 | framed protocol, idempotent launch, adoption, lease guard, disconnect enforcement (`internal/runner/hc` and `test/real`) | Linux (no herdr-core in the Linux image) |
| Claude Code 2.1.x | real runs using SpaceMan tools, the gateway and transcripts (Haiku, budget-bounded); a real worker in the demonstration | — |
| Codex | — | no credential; every Codex run is simulated |
| Porch OIDC | sign-in through a real go-porch v0.3.0 (PKCE, groups mapping, step-up) with its fake Google upstream | real Google behind Porch; TLS deployment |
| Pulse, NATS, go-pulse-github | real pulsed v0.1.3 in operator mode, nats-server 2.15.1, real pulse-github v0.1.0 with its fixtures; outbound publication read back | real GitHub; pulsed's production TLS profile |
| S3 | MinIO over TLS: backups, verify, tamper detection, offline restore; the S3 artifact store (attachments uploaded, downloaded with ranges, backed up, migrated, restored) | AWS S3, R2; a node's recording and transcript in a real bucket |
| Linux, systemd | release archive under systemd 257 in a Debian 13 container (arm64) | a physical or cloud host; amd64 under systemd |
| Containers | `make image-smoke` | release workflow, published images |
| Browsers | Chromium, Firefox, WebKit | — |

Per-requirement evidence, with the simulated and real columns kept apart, is in [TRACEABILITY.md](TRACEABILITY.md) and
[TEST_STRATEGY.md](TEST_STRATEGY.md).

## 7. Limitations and blockers

The full list is [LIMITATIONS.md](LIMITATIONS.md). The ones an evaluator should know first:

- **Trusted teams only.** SpaceMan is not a hostile multi-tenancy boundary; folder grouping is not isolation, shell access
  can bypass MCP mediation, and privileged nodes can read runtime secrets ([THREAT_MODEL.md](THREAT_MODEL.md)).
- **Single central, no HA.** Recovery is restore from backup, which comes up paused.
- **Codex has never run a real turn.** Its skills and its login layout are verified against codex-cli 0.166.0 without
  a credential; a real ChatGPT login and a real model turn are not.
- **1 partial requirement**, SM-VER-002 (real-dependency verification), with its gap in
  [TRACEABILITY.md](TRACEABILITY.md): no real Codex run, AWS S3/R2, physical Linux host, or real Google and GitHub
  accounts.
- **Open security findings**: the review fixed all 4 medium and 7 of 14 low findings; 6 low and 5 informational
  findings are open with follow-ups and 3 are accepted with a rationale ([SECURITY_REVIEW.md](SECURITY_REVIEW.md)). There
  were no high findings. The review was a code reading, not an external audit or penetration test.
- **Agent restarts under systemd** end the node's runs (`KillMode=control-group`), so adoption after an agent restart
  does not apply under the shipped unit.

## 8. Deployment and recovery

On the site (Operators section):

- **Install**: binaries and systemd, containers and the image matrix, or from source (Installation; Containers and
  images). Central needs a KEK file outside the data directory; production requires TOTP for administrators.
- **Configure**: the generated Configuration reference; the example configs in `deploy/examples/`.
- **Upgrade and roll back**: Upgrade and rollback (stop, back up, replace binaries, start; migrations are forward-only).
- **Recover**: Deployment overview has the recovery map (restore, lost KEK, failed upgrade, a node gone, lockout);
  Backups and restore covers `spaceman backup create|verify|restore`; Relocating a space covers moving a space off a
  failed node, including the unavailable-source flow.
- **Troubleshoot**: Troubleshooting lists the real startup errors, readiness codes and node-connection problems.

## 9. Website build and publication status

The site builds and passes `make site-check` (dead links, screenshots, Mermaid, axe). It is **not published**: there is
no remote repository and the Pages workflow (`.github/workflows/pages.yaml`) has never run. The site's Publishing page
explains how to publish it.

## 10. Screenshot coverage and refresh

67 screenshots in [SCREENSHOTS.md](SCREENSHOTS.md), every one current, captured from a real stack (central, agent,
isolated herdr-core core, stub harness) in both themes, each used by at least one page. Refresh with
`(unset HC_TERMINAL_ID HC_TERMINAL_SOCKET HC_SCOPE; make screenshots)`, review the changed images, and revert the ones
whose content did not change. The procedure is on the site's Screenshots page. Screenshots show the stub harness, not
real model output.

## 11. Requirement traceability

[TRACEABILITY.md](TRACEABILITY.md): 220 requirements, each with its work package, status, named tests, real evidence and
notes; `make traceability` checks that every requirement is listed and that the SM-SEC-001 boundaries are documented.
Status: 218 implemented, 1 partial, 1 n/a. The brief's §40 verification topics are mapped to tests in
[TEST_STRATEGY.md](TEST_STRATEGY.md); the §41 demonstration is [DEMO.md](DEMO.md).

## 12. Before a production release

1. Run central and an agent on a physical Linux host (amd64 and arm64) under the shipped systemd units, including an
   agent restart and herdr-core for Linux.
2. Verify Codex with a real credential, including skills (or record that Codex has none).
3. Close or accept the open security findings, ideally with an external review.
4. Push to a remote, run the release workflow, publish images, SBOMs and checksums, and publish the site.
5. Collect the missing real evidence (SM-VER-002), at least a real Codex run, or state the gaps in the release notes.

## 13. Addendum: Harbor Pilot gateway (PR #17, HP-T8, 2026-11-08)

Written for the combined pull request that adds the Harbor Pilot gateway (ADR-0060, **Accepted** 2026-11-08 with Q4
open). Sections 1 to 12 above are the 2026-11-03 report and are not restated. **No production claim:** every real run
below used Harbor Pilot's own demo gateway with synthetic upstreams. A real model provider behind a real Harbor Pilot has
**not** been used (Q7: the operator will provide one).

**Release impact.** Migrations 0040 (connection, space settings, run tokens, revocation queue) and 0043 (run-end usage
reads), forward-only; 0042 is retired and never used. Node protocol minor 9: only spaces that opt in need updated
agents; a gateway launch to an older agent fails as `agent_too_old` before a token is minted. Minimum Harbor Pilot
0.1.3. The Terraform resources come in the provider's next release (provider PR #12), which needs this release.

**Evidence labels.** *contract-tested (fake)*: automated tests against `internal/harborpilot/hpfake` or a stand-in, run
by `make check` and CI. *real-demo-tested*: the opt-in suites against a real `harbor-pilot serve --demo` **0.1.3**
binary (`go install`, checked by buildinfo), run on 2026-11-08 for this addendum. *not verified*: no evidence.

| Done-when item (operator request, 2026-11-06) | contract-tested (fake) | real-demo-tested (serve --demo 0.1.3) | not verified |
|---|---|---|---|
| Claude Code and Codex runs each get their own token | `TestRunTokenMintOnceAndReuse`, `TestRunTokenSingleFlight`, `TestClaudeGatewayBuild`, `TestCodexGatewayBuild`, `TestHarborPilotGatewayDeliveryOnNode` | `TestRealHarborPilotGatewayRuns` (`make test-real-harbor-pilot`: real Claude Code and codex-cli through central, agent, herdr-core and the shim) | — |
| Their requests show up under the SpaceMan client with trusted refs | `TestRunTokenMintOnceAndReuse` (bound refs sent) | `TestRealHarborPilotRunToken` (bound refs on the credential), `TestRealHarborPilotGatewayRuns` (request detail: the binding, `spaceman.space`, `spaceman.run`, `session`; one session per run) | requests to a real provider |
| Tokens are revoked at run end | one test per end path in `internal/fleet/gateway_test.go`, `TestRevokerOutcomes`, `TestRevocationSurvivesRestart`, `TestHarborPilotRevocationAfterRestart` | `TestRealHarborPilotRunToken` (401 after the reported end), `TestRealHarborPilotGatewayRuns` (401 after a stop, by `serve`'s own timer) | relocation, purge and lease loss against the real gateway (fake only) |
| Emergency stop: revoking the parent cuts every run (T-39) | `TestParentRotationRevokesLiveRunTokens` | `TestRealHarborPilotRunToken` (a live run's token gets 401 at once; the next launch fails as `gateway_unavailable`) | — |
| Never a silent fallback to a direct account | `TestRouteAndPreLaunch`, `TestHarborPilotRunTokenLifecycle`, `TestGatewayLaunchNeedsAgentMinor` | `TestRealHarborPilotRunToken` (revoked parent: the run fails, no account used) | — |
| A space with an exhausted budget can't launch and says why | `TestCheckBudgetRefusesUsedUpBlockingBudget`, `TestCheckBudgetStatusUnavailable`, `TestCheckBudgetOwnFailureFollowsPolicy`, `TestHarborPilotBudgetRefusesLaunch`, `TestHarborPilotBudgetRefusalRaisesControlCouldNotStart` | `TestRealHarborPilotBudgetAndUsage` (a used-up tiny budget: the refusal names it, its spend and reset; Harbor Pilot refuses the `check_budget` mint) | the "Control could not start" path against the real gateway |
| The Limits page shows Harbor Pilot budget status | `HarborPilotLimitsCard.test.tsx` (ok, warning, exceeded, unavailable, error), `TestSpaceStatusStates`; screenshots against a stand-in gateway | the status API in `TestRealHarborPilotBudgetAndUsage` | the page rendered against a real gateway |
| Events arrive as inbox items for the right space | `TestHarborPilotTemplates`, `TestHarborPilotEventsReachTheRightSpace` (fixtures captured from the real demo) | `TestRealHarborPilotEventsThroughPulse` (Harbor Pilot's relay into a Pulse-shaped JetStream test server) | a real `pulsed`; a live `quota.window.*` event; routing of unscoped events (Q4 open) |
| Codex usage filled only with real data, source labelled | `TestReadDueUsageFillsUnavailableUsage`, `TestReadDueUsageKeepsHarnessUsage`, `TestReadDueUsageFailures`, `TestReadDueUsageEmptyIsNotZero`, `TestHarborPilotRunUsageReadBack` | `TestRealHarborPilotBudgetAndUsage` (a run's two requests read back, source `harbor-pilot`) | usage of a real provider |
| Terraform acceptance tests pass, the combined example included | provider unit tests | provider PR #12 `make acc-harbor-pilot`: `TestAccHarborPilot`, `TestAccHarborPilotExample` (with the `harborpilot` provider), 26/26 | a production deployment |
| Tests run against a real `serve --demo` binary | — | every row above; the plan named 0.1.1, the pinned minimum is now 0.1.3 | — |
| A real upstream model provider behind Harbor Pilot | — | — | **not verified** (Q7) |
| Status recorded honestly (contract-tested vs live-verified) | — | — | this table; [TRACEABILITY.md](TRACEABILITY.md) SM-GW-001…010 keep the columns apart |

**Gates for this addendum** (2026-11-08, Linux, the head named in PR #17): `make check` (umask 022,
`SM_TEST_TX_MAX=500ms`), `make web-check`, `make site-check`, `SPACEMAN_HARBOR_PILOT=1 make test-harbor-pilot`,
`make test-real-harbor-pilot` and CI. Results are in the PR description and PLAN §3.

**Open:** Q4 (unscoped event routing, DECISIONS K-12); a run against a real provider (Q7); the limitations in
[LIMITATIONS.md](LIMITATIONS.md#harbor-pilot-gateway), first among them that a Harbor Pilot outage stops every
opted-in space's routed harnesses, by design (no fallback).

## 14. v0.1.0 final (2026-11-08)

The operator authorized the final v0.1.0 release on 2026-11-08. It is cut from the merge of PR #18 into `main` and
tagged `v0.1.0`; the release workflow builds the archives, images and SBOMs into a draft release that the operator
publishes. It still makes **no production-readiness claim**: §1's verdict stands (ready for evaluation by a trusted
team), now with the release workflow as the path that builds and publishes it.

**Contents since v0.1.0-rc.9:** the Harbor Pilot gateway (ADR-0060, §13), the SpaceMan admin MCP and the external
`/api/v1/mcp` endpoint (ADR-0063, Accepted), Claude Code's classic renderer by default (ADR-0064), control
reminders (ADR-0065), the terminal-prompt deny (ADR-0066), the System page (ADR-0067), the deny-only permission hook
with instance protection and the auto-memory relocation (ADR-0068), and the store busy/close fix. The full list is in
`CHANGELOG.md` `[0.1.0]`.

| Item | v0.1.0 |
|---|---|
| Migrations since rc.9 | 0040–0045, forward-only: 0040 Harbor Pilot connection, space settings, run tokens and revocation queue; 0041 `messages.deliver_at`; 0042 retired, never used; 0043 Harbor Pilot run-end usage reads; 0044 SpaceMan MCP servers and internal tokens; 0045 the partial index `ix_runs_observed_live`. Take a backup before upgrading; central also writes a pre-migration snapshot. |
| Node protocol | 1.9 (`proto.MaxMinor = 9`; minor 9 is the Harbor Pilot gateway token). Agents back to 1.0 still connect; a gateway launch to an agent older than 1.9 fails as `agent_too_old`, and the node-side Claude Code changes (ADR-0064, 0066, 0068) take effect only on agents of this release. |
| Claude Code tested | 2.1.317: the real Claude Code suites, `make test-real-claude-cli` (ADR-0068 deny rules, 52 instances, the auto-memory migration) and the Harbor Pilot gateway runs. |
| Codex tested | codex-cli 0.166.0 without a real model credential (§7). |
| Harbor Pilot | 0.1.3 or later, verified against `serve --demo` only (§13). |

**Upgrade notes from rc.9** (also in the CHANGELOG):
- Upgrade central first, then every node agent (site page Operators → Upgrade and rollback).
- Each instance's Claude Code auto memory moves to `<data_dir>/automemory/<space>/<instance>/…` and is copied there at
  the instance's first launch on this release (the old copy stays in place).
- Deny rules protect every space's instance files from a run's file tools, also when a profile adds the whole
  `spaces/` folder.
- A `PermissionRequest` hook denies every permission dialog; an `ask` rule now works like a deny.
- A profile's own `autoMemoryDirectory` is ignored (the node logs a warning).
- Closing the store waits at most 8 s (WAL checkpoint and connections still in use), inside the units'
  `TimeoutStopSec=45s`.
- Operators should remove any profile `additionalDirectories` entry that grants the node's whole `spaces/` folder, and
  any dialog-denying hook script: v0.1.0 adds the run's own space folder and the hook automatically.
- If a node ran Codex with an API-key account on an earlier release, rotate that key.

**Known issues it ships with** (open in [LIMITATIONS.md](LIMITATIONS.md); the §7 list still applies):
- No real Codex model turn, no AWS S3/R2, no physical Linux host or amd64 under systemd, no real Google or GitHub
  accounts (SM-VER-002 stays `partial`).
- Harbor Pilot: no real model provider behind it (Q7); unscoped events are routed nowhere (Q4); an outage stops every
  opted-in space's routed harnesses, by design (no fallback).
- SpaceMan MCP: no real harness has driven the tools through a model yet (tested in process over the real bridge);
  the Terraform resource waits for the provider's next release; the external endpoint takes bearer tokens only (no
  OAuth).
- Claude Code runs: an `ask` rule works like a deny; the protected paths and stripped settings keys are deny-lists
  checked against Claude Code 2.1.317 (an allow-list is a follow-up); a run can read other instances' transcripts,
  settings and memory.
- Agent restarts under the shipped systemd unit end the node's runs.

**Accepted risks** (operator decisions, [THREAT_MODEL.md](THREAT_MODEL.md), [SECURITY_REVIEW.md](SECURITY_REVIEW.md)):

| ID | Risk | Decision |
|---|---|---|
| T-33 | An agent with SpaceMan MCP tools uses its owner's administrator authority (default preset `admin@*`, no expiry, approvals for dangerous tools only). | Accepted 2026-11-07 for a trusted infra space. |
| T-38 | An operator's own MCP client on `/api/v1/mcp` uses that token's full grants; reads and writes run without approval. | Residuals accepted 2026-11-08 (F6, SR-MCP-01). |
| T-40 | Dialog handling: `ask` rules become denies; reads of other instances' files; deny-lists tied to the tested Claude Code version; a profile editor can still widen a run. | Residuals accepted with ADR-0068. |
| F1 | In-run dangerous calls are approved in the run's own space, also when they act on other spaces. | Accepted 2026-11-08. |
| F2 | `launch-run` stays a write with `settings.permission_mode` or `account_ref` overrides. | Accepted 2026-11-08. |
| F6 | `/api/v1/mcp` is on for every user API token, with no switch. | Accepted 2026-11-08. |
| SR-MCP-01 | The root token may approve the external endpoint's dangerous calls. | Kept 2026-11-08 (single-administrator path). |

**Gates** (Linux, umask 022, `SM_TEST_TX_MAX=500ms`, at the PR #18 head named in the PR description):
`make check`, `make site-check` and CI on PR #18, all passing before the PR left draft; the results and the head
commit are in the PR description.

## 15. v0.1.1 (2026-11-09)

The operator authorized the v0.1.1 release on 2026-11-09. It is cut from `main` after PRs #19–#33 were merged
in order (the release-prep PR follows them). It is tagged `v0.1.1`, and the release workflow builds the archives,
images and SBOMs into a draft release that the operator publishes. It still makes **no production-readiness claim**:
§1's verdict stands.

**Contents since v0.1.0:**
- concurrency limits that count live runs only, and a stale-run sweeper (ADR-0074, migration 0046);
- per-run terminal sizes (ADR-0075, SM-UI-007, migration 0047, protocol minor 10);
- attachments of any size reaching a run as a file (ADR-0073, T-42);
- `github-pull-request` version 2, and built-ins that gain versions on existing installations (ADR-0072);
- Harbor Pilot 0.1.4 support;
- the web terminal lifecycle and lagged-viewer fixes, the scrollbar fixes, and the chunk-load recovery;
- gate and CI changes: tiered gates and a one-run coverage floor (ADR-0069, T-41), the write-transaction guard
  measuring lock hold time (ADR-0070), and the login timing comparison running in its own job (ADR-0071);
- flaky-test fixes.

The full list is in `CHANGELOG.md` `[0.1.1]`.

| Item | v0.1.1 |
|---|---|
| Migrations since v0.1.0 | 0046 (the disconnect grace on each lease) and 0047 (a run's terminal size). Both are forward-only. Take a backup before upgrading; central also writes a pre-migration snapshot. |
| Node protocol | 1.10 (`proto.MaxMinor = 10`; minor 10 is run terminal sizes). Agents back to 1.0 still connect; their runs can't be resized (the API answers 409). |
| Configuration | `nodes.lease_ttl` must be at most 10m (ADR-0074); central refuses to start with a longer value. |
| Claude Code tested | The real Claude Code suites last ran against 2.1.317 (v0.1.0). The new live attachment test, `TestRealClaudeCLIReadsSavedAttachment`, ran against 2.1.319. |
| Harbor Pilot | 0.1.3 or later; 0.1.4 recommended. Verified against `serve --demo` only (§13). |

**Upgrade notes from v0.1.0** (also in the CHANGELOG):
- Upgrade central first, then every node agent.
- Check `nodes.lease_ttl` (and `SPACEMAN_NODES_LEASE_TTL`) is at most 10m.
- The first deploy's stale-run sweep ends every historic run that central still counted as live but whose process is
  gone. It revokes their credentials and gateway tokens and reports them as ended. For worker runs, the space's
  control may then re-queue or relaunch their tasks. Review the `run.reconciled_stale` audit events and close the
  tasks you no longer want.

**Known issues it ships with:** the §14 list still applies (see [LIMITATIONS.md](LIMITATIONS.md)). In addition:
- The CI `login timing` job runs with the default 100 ms write-transaction guard, which can trip on hosted runners
  (seen once while preparing this release, passing on rerun).
- `make e2e` takes about 13 minutes longer because of the UI overflow guard.

**Accepted risks:** unchanged from §14.

**Gates** (Linux, umask 022, at `main` after the last merge, PR #32): `SM_TEST_TX_MAX=500ms make check`,
`make web-check`, `make e2e` and `make site-check`, plus CI on every merged PR head. The results are in the
release-prep PR description.

## 16. v0.1.2 (2026-11-13)

The operator authorized the v0.1.2 release on 2026-11-13. It is cut from `main` after PRs #35–#45 were merged in
order (#38 first, #45 before #44; the release-prep PR follows them). It is tagged `v0.1.2`, and the release workflow
builds the archives, images and SBOMs into a draft release that the operator publishes. It still makes **no
production-readiness claim**: §1's verdict stands.

**Contents since v0.1.1:**
- node-managed Claude Code versions (ADR-0077, SM-HARN-005…008): a verified per-version cache, launches by absolute
  path, per-profile version pins, per-run versions and the current versions on each node (migrations 0048 and 0049,
  agent journal migrations 0006 and 0007, protocol minor 11);
- Claude Code's self-update turned off in every run, managed or not;
- the web terminal's fresh-cache freeze and stale-glyph fixes (ADR-0076);
- the New inbox item dialog's scrollbar with writing-assistant extensions;
- Go 1.27.4 (GO-2026-6859);
- CI: the write-transaction guard at 250 ms for the hosted login-timing and fuzz jobs and 1 s for the hosted coverage
  step (ADR-0062 updates).

The full list is in `CHANGELOG.md` `[0.1.2]`.

| Item | v0.1.2 |
|---|---|
| Migrations since v0.1.1 | Central: 0048 (a profile's Claude Code version pin and each run's version) and 0049 (when a node last reported its capabilities). Node agent journal: 0006 (each run's Claude Code version and path) and 0007 (an index for the cache's GC). All are forward-only. Take a backup before upgrading; central also writes a pre-migration snapshot. |
| Node protocol | 1.11 (`proto.MaxMinor = 11`; minor 11 is pinned launches, per-run versions and capability re-reports). Older agents still connect; a pinned launch to one fails as `node_too_old`, and they report harness versions only when they connect. |
| Configuration | New node agent settings `harness.claude.{manage, channel, base_url, cache_keep, cache_keep_pinned, refresh, download_timeout, self_update}`. `manage` is off by default. Claude Code's self-update is off in every run unless `self_update: true` on an unmanaged node. |
| Disk | A managed node's cache takes about 340 MB per Claude Code version: about 1 GB with the default `cache_keep` (3), plus up to `cache_keep_pinned` (5) pinned versions. |
| Web terminal | The terminal's WebAssembly module comes from the pinned ghostty-web prerelease 0.4.0-next.21; its JavaScript stays on 0.4.0 (ADR-0076). |
| Go | 1.27.4 (GO-2026-6859), in `go.mod`, `tools/go.mod` and both Dockerfiles' build image. |
| Claude Code tested | The real Claude Code suites last ran against 2.1.317 (v0.1.0) and the live attachment test against 2.1.319 (v0.1.1). The ADR-0077 spike checked 2.1.322's updater and resuming a 2.1.317 session on 2.1.322 in print mode against a fake API. |

**Upgrade notes from v0.1.1** (also in the CHANGELOG):
- Take a backup, then upgrade central first, then every node agent.
- Nodes whose `claude` stayed current only through Claude Code's self-update stay on their current version until
  `harness.claude.manage: true` is set or they are updated with your own tooling.
- On managed nodes, size the agent's data directory for the cache, and check `harness.claude.*`. A profile's
  `harness_version` pin is honoured: an exact pin is downloaded and never falls back to another version.

**Known issues it ships with:** the §15 list still applies (see [LIMITATIONS.md](LIMITATIONS.md), including Harness
distribution). In addition:
- Downloaded Claude Code binaries are checked against the manifest's SHA-256 and size only, not a signature (K-14).
- Hosted CI runners intermittently held SQLite's write lock for 260–750 ms in untouched packages; several merges
  needed one rerun of the coverage or race job, and the hosted coverage step now uses 1 s (ADR-0062 update).

**Accepted risks:** unchanged from §14.

**Gates** (Linux, umask 022, at `main` after the last merge, PR #44): `SM_TEST_TX_MAX=500ms make check`,
`make web-check`, `make e2e` and `SM_SITE_A11Y=1 make site-check`, plus CI on every merged PR head. The results are
in the release-prep PR description.
