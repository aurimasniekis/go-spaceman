# Release readiness report

> Work package M5-05 (GOAL §8). Written 2026-11-03 at the head of `main` after the commits listed below.
> This report makes **no production-readiness claim**. SpaceMan is feature-complete against the brief with the gaps
> recorded here and in [LIMITATIONS.md](LIMITATIONS.md). It has not been released, published or run by anyone but its
> developer, and several dependencies have only simulated evidence.

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

57 ADRs in [DECISIONS.md](DECISIONS.md). The ones that shape operation most:

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
