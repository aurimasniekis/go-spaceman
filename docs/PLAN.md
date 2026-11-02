# SpaceMan — Execution Plan

> Product contract: `docs/GOAL.md`. Decisions: `docs/DECISIONS.md`. Evidence: `docs/TRACEABILITY.md`.
> Status vocabulary: `todo`, `in progress`, `implemented`, `tested (simulated)`, `verified (real)`, `blocked`.
> A work package is `done` only when it meets GOAL.md §7.

## 0. Working references

- Original implementation prompt (working reference only, never committed). Read-only copy saved 2026-11-02:
  `/private/tmp/claude-501/-Users-aurimas-Projects-Golang-spaceman/a1d9e8b2-8d7b-427a-8d5a-3c9b4ddfc95f/scratchpad/spaceman-prompt.z4Sibr/spaceman-implementation-prompt.md`
  (1,499 lines, 47,272 bytes, sha256 `d326fe0433e0b934269f6a87c378fb128a3763d6438d78b16aa44bf90a8b38a3`). This path is session-scoped
  temporary storage and may disappear. GOAL.md is the durable record of the requirements.
- Dependency contracts: `docs/contracts/`.
- Sibling checkouts used for conventions: `../hallpass` (v0.7.0), `../pulse` (v0.1.3), `../pulse-github` (v0.1.0), `../porch` (v0.3.0).

## 1. Milestones

### M1 — Foundation and first real run
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M1-01 | Repo skeleton: go.mod, Makefile, lint, CI workflows, buildinfo, licence files | SM-WORK, SM-PKG-001 | implemented (CI web/pages/release jobs land with their dirs) |
| M1-02 | Store: SQLite pools, data-dir lock, goose migrations, sqlc, IDs, clock, keyring, audit chain | SM-STORE-001, SM-OBS-003, SM-INV-001 | tested (simulated) |
| M1-03 | API: huma, problem errors, idempotency, If-Match, pagination, change feed + SSE watch; management listener | SM-API-002, SM-OBS-001/002 | tested (simulated) + real smoke |
| M1-04 | Auth: root-token bootstrap, first admin, Argon2id login, TOTP, sessions, CSRF, throttle, API tokens, Casbin | SM-AUTH-001/003/004/005/006 | tested (simulated) + real smoke; OIDC in M4-01 |
| M1-05 | Node protocol: enrollment, connect, hello/capabilities, heartbeat, leases, fencing, node journal | SM-NODE-001…007 | tested (simulated) + real (`TestRealNodeEndToEnd`, isolated hc core) |
| M1-06 | hc runner: contract spikes K-1/K-2/K-3, client, idempotent launch, adoption, reconcile | SM-RUN-001/002, SM-NODE-004/008/009 | runner done (real-verified); node-side reconcile with M1-05 |
| M1-07 | Harness adapters: Claude Code + Codex, isolated dirs, launch shim, MCP bridge (minimal tools) | SM-HARN-001/002/003, SM-ACCT-004 | implemented; real harness runs need a credential |
| M1-08 | Spaces: CRUD, pinning, pause modes, archive/restore/purge; launch a run | SM-SPACE-001…004 | tested (simulated) |
| M1-09 | Recording: zstd segments, upload, live view, replay | SM-REC-001…004 | tested (simulated) + real; UI replay/live in M1-11 |
| M1-10 | Restart/disconnect reconciliation tests (central, node, hc) | SM-NODE-005/007/009 | tested (simulated + real): agent/central restarts, crash between start and journal, exit while down, disconnect enforcement; stale-generation fencing test with relocation (M4) |
| M1-11 | Web shell: Night Owl theme, bootstrap/sign-in, spaces, nodes, overview, terminal | SM-UI-001/002/004 | tested: vitest + browser journey (`make e2e`, real agent + isolated hc) |
| M1-12 | Site skeleton, Mermaid, demo seed, Playwright screenshots, coverage inventory | SM-DOCS-001…005 | implemented: site with guides for implemented features, 12 real shots × 2 themes, CI site job, Pages workflow (not published); axe checks pending |

### M2 — Coordination core
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M2-01 | Attachments + TipTap composer | SM-ATT-* | implemented: storage, API, composer, sanitizing renderer; MCP access with M2-06 |
| M2-02 | Inbox (operator text/JSON/attachments, sources, statuses) | SM-INBOX-* | implemented: service, API, UI, live updates; other producers with their WPs |
| M2-03 | Pulse ingestion: connections, durable consumers, dedup, quarantine, fan-out, metrics | SM-EVT-011…016 | implemented (real SDK on an in-process NATS server; UI; e2e via devpulse); metrics and NATS Core pending |
| M2-04 | Event templates + subscriptions + GitHub presets + UI preview/testing | SM-EVT-001…010, 017 | implemented: engine, catalog, subscriptions, preview, UI, guide and screenshots |
| M2-05 | Execution queue: ordering, dependencies, exclusions, eligibility, incidents, demotion | SM-QUEUE-* | implemented: service, API, UI with drag and keyboard sorting, guide, screenshots |
| M2-06 | Control + workers + worktrees + durable messages + delivery adapters | SM-CTRL-*, SM-COMM-*, SM-REPO-* | implemented: agent tool gateway, durable messages and notices, control orchestration tools, per-worker git worktrees, control notifications and bounded recovery |
| M2-07 | Human actions: default types, lifecycle, MCP, reply routing; per-space + global UI | SM-HUM-*, SM-HUI-* | implemented (MCP tools and notification delivery with M2-06): service, API, global and per-space UI, guide, screenshots |
| M2-08 | Docs + screenshots for M2 | SM-DOCS | todo |

### M3 — Configuration, intelligence, memory
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M3-01 | Accounts + encrypted secrets + resolution | SM-ACCT-* | implemented (pulled forward for real runs): service, API, Accounts page, space bindings, launch choice, real Claude run; Codex ChatGPT login open (K-9) |
| M3-02 | Prompts catalog, harness profiles, role templates, resolution, manifests | SM-PROMPT-* | todo |
| M3-03 | MCP gateway (upstreams, grants, limits, SSRF) + skills incl. Hallpass | SM-MCP-* | todo |
| M3-04 | Human-action type catalog, custom schemas, schema forms | SM-HTYPE-* | todo |
| M3-05 | Enforced approvals | SM-APPR-* | todo |
| M3-06 | Memory collections + sync | SM-MEM-* | todo |
| M3-07 | KB repositories + support sessions | SM-KB-*, SM-CTRL-005 | todo |
| M3-08 | Dreamer | SM-DREAM-* | todo |
| M3-09 | Transcripts, artifacts, usage | SM-REC-005 | todo |

### M4 — Hardening
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M4-01 | OIDC/Porch, linking, recovery codes, step-up, stream revocation | SM-AUTH-001/002/007 | todo |
| M4-02 | Quotas, limits, schedules, replay | SM-LIMIT-*, SM-EVT-015 | todo |
| M4-03 | Outbox → Pulse publication | SM-OBS-004/005 | todo |
| M4-04 | Backups + restore | SM-STORE-* | todo |
| M4-05 | Relocation (clean + dirty refusal + unavailable source) | SM-SPACE-005…008 | todo |
| M4-06 | Cross-space requests | SM-XSPACE-* | todo |
| M4-07 | Human response failure handling, reminders, escalation | SM-HUM-009/011 | todo |
| M4-08 | Fault tests + security review | SM-SEC-*, SM-VER-001 | todo |
| M4-09 | Packaging: Dockerfiles, systemd, release script, SBOM, checksums | SM-PKG-* | todo |

### M5 — Completion
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M5-01 | Terraform provider (nested module) | SM-API-004/005 | todo |
| M5-02 | CLI + API completeness, generated reference | SM-API-003 | todo |
| M5-03 | Complete site + all screenshots current | SM-DOCS-* | todo |
| M5-04 | End-to-end demonstration script + real evidence | SM-VER-002/003 | todo |
| M5-05 | Release readiness report | GOAL §8 | todo |

## 2. Phase checklist (run at the start and end of each WP)

1. Reread the relevant GOAL.md requirements and the contracts each one touches.
2. Update the WP status here and the evidence in TRACEABILITY.md.
3. Record deviations and newly found unknowns in DECISIONS.md.
4. Check the invariants (GOAL §3): pinning, single control run, fencing, ordering authority, no polling per idle space.
5. Commit through `/cc`, and record the hash in §3.

## 3. Progress log

| Date | WP | Summary | Commits | Evidence |
|---|---|---|---|---|
| 2026-11-02 | M1-01 | Planning: prompt saved, contracts inspected, tracking docs created | `625719d` (root commit, see §4 D-1) | gitleaks: no leaks |
| 2026-11-02 | M1-06 | herdr-core spikes S-0..S-2 against an isolated core; ADR-0009/0017 | `1bbee9e` | real hc 0.1.16 |
| 2026-11-02 | M1-01/02 | Foundation: store, keyring, audit, obs, config, txscope, Makefile, CI | `afe13a8` | `make check` green |
| 2026-11-02 | M1-04 | Auth services: users, Casbin authz, root token, scoped API tokens | `9cd4a30` | auth 82.3%, authz 87.8% coverage |
| 2026-11-02 | M1-03 | HTTP API (huma), rules table, CSRF/origin, If-Match, idempotency, SSE watch; serve/admin/keys/openapi CLI | `3b1d448` | e2e tests + real `serve` smoke |
| 2026-11-02 | M1-06 | K-1 framed protocol decoded from real traffic (S-3) | `c959244` | real hc 0.1.16 |
| 2026-11-02 | M1-06 | Runner interface + herdr-core framed client + core manager | `b8b70d7` | unit (fake core, -race) + real suite (`-tags real`) |
| 2026-11-02 | M1-05/07/08 | Node enrollment, pinned spaces, leases with fencing, the node agent (journal, exec, lease guard, MCP bridge), harness adapters | `80d6372` | `TestFleetLifecycle`, `TestDisconnectEnforcement`; real `TestRealNodeEndToEnd` |
| 2026-11-02 | M1-05 | Harness capability detection before dialing central | `1519f0f` | real node e2e |
| 2026-11-02 | M1-09 | Recording shipped to central; live terminal SSE; replay/download/delete API | `4d979a7` | `TestRecordingShippedReplayedAndStreamed`; real node e2e |
| 2026-11-02 | M1-11 | Web UI (Mantine 8 + Night Owl, TanStack Router/Query/Form, ghostty-web live/replay terminal), UI CSP (ADR-0020), lease grants in the change feed, dev stack + browser journey | `9b02552`, `c32e0d9` | `make check`, `make web-check`, `make e2e` (Chromium, real agent + hc 0.1.16 isolated core, stub harness): pass; screenshots reviewed, not committed (M1-12) |
| 2026-11-02 | M1-10 | Restart reconciliation: crash between program start and journal (adoption), exit while the agent is down, central + agent restart; real central restart and real disconnect enforcement | `dc22aaa` | `TestRestartReconciliation` 5× `-race`; `-tags real` `TestRealNodeEndToEnd`, `TestRealDisconnectEnforcement` pass (hc 0.1.16 isolated core) |
| 2026-11-02 | M1-04 | Reset and recovery tokens were not redeemable (services without HTTP/UI): `/auth/reset`, `/auth/recover`, recovery-code regeneration, break-glass alert; `/reset` and `/recover` pages | `8ea777a` | `TestAPIRecoveryAndReset`; `make e2e` covers a password reset |
| 2026-11-02 | M1-12 | VitePress site (Porch setup) with guides for implemented features, generated API reference, README, real screenshots (`scripts/screenshots.sh`), screenshot/Mermaid checks, CI site job, Pages workflow | `9ebab24` | `make site-check` (build with dead-link check, 7 Mermaid diagrams, API drift); screenshots reviewed |
| 2026-11-02 | M2-01 | Attachments: content-addressed local store, sniffed types, quota and size limits, references, GC, multipart upload, safe serving | `8135106` | `internal/attachments` tests, `TestAttachmentsOverHTTP` |
| 2026-11-02 | M2-01/02 | Inbox service, API and UI; TipTap composer with paste/drop/attach; sanitizing Markdown renderer; inbox screenshots and guide | `7e79bb8` | `internal/inbox` tests, `TestInboxOverHTTP`, `markdown.test.ts`; `make e2e` (composer → item → done); screenshots reviewed |
| 2026-11-02 | M2-04 | Event template engine, GitHub seeds from the go-pulse-github contract, operator-mode NATS test server | `ebe8c8d` | `internal/events/template` 88.6%; real SDK durable round trip |
| 2026-11-02 | M2-03/04 | Connections (sealed creds), template catalog with immutable versions and follow-latest, subscriptions with explicit cursors, ingestion manager (one durable per subscription), delivery history, quarantine with reprocess, events API | `a1c4482` | `internal/events` integration tests ×3 `-race` (fan-out, filter, dedup, quarantine, replay/start-now), `TestEventsOverHTTP` |
| 2026-11-02 | M2-04 | Events UI (catalog, template testing, connections, subscribe form, subscription page with grant, cursor, deliveries, quarantine), devpulse in the dev stack, events guide and screenshots | `d14ba20` | `make e2e` (connection → subscription → published event → inbox item); screenshots reviewed |
| 2026-11-02 | M2-05 | Execution queue: insertion by priority/policy, authoritative moves, dedup that never re-promotes, escalation by policy, dependencies (cycles rejected), blockers, exclusions, active limit, selection with skip reasons, states with preemption; API, UI with drag and keyboard sorting, create task from inbox | `9bb3c22` | `internal/queue` tests, `TestQueueOverHTTP`, `make e2e` (inbox → task, Alt+↑, start) |
| 2026-11-02 | M2-07 | Human actions: question, approval (advisory), to-do and message types with distinct responses; lifecycle separate from outcome; atomic finalization; claim, comments, withdraw, expiry, follow-ups, dedup and limits; blocking only the related task; durable requester notifications; Needs you page with nav badge, per-space tab, safe bulk actions | `d7f295a` | `internal/human` tests (84%), `TestHumanActionsOverHTTP`, `make e2e` (blocking question answered) |
| 2026-11-02 | M1-07 | Spike S-5 with a setup-token: interactive Claude needs first-run state and folder trust seeded; shim scrubs inherited session markers and node credentials (ADR-0021, K-10 resolved) | `8d5cf8b` | `TestScrubEnv`, `TestMergeJSONFile`, `TestClaudeBuild` |
| 2026-11-02 | M3-01 | Accounts: sealed write-only credentials, kinds per harness, space bindings with defaults, deterministic resolution at launch, current secret at dispatch, unusable account fails the run; Accounts page, space card, launch choice | `bb70d0f` | `internal/accounts` tests (85.8%), `TestFleetLifecycle`, `make e2e`, `test/real` `TestRealClaudeRun` (real Claude Code, Haiku) |
| 2026-11-02 | M2-06 | Agent tool gateway: protocol minor 1 tool calls, central authentication per call, catalogs per run kind, inbox/task/attachment/human tools; one sealed token per run (ADR-0022); agent inbox items audited | `96926bd` | `internal/agenttools` tests (86.7%), `TestAgentToolsOverBridge` |
| 2026-11-02 | M2-06 | Durable messages (stored/available/acknowledged/resolved, requests and replies, dedup), notifier delivering content-free notices through idle-gated input, human responses delivered the same way, messaging tools, operator Messages tab | `e8d05f9` | `internal/messages` tests (83.6%), `TestMessagesDeliveredToIdleRuns`, `make e2e` |
| 2026-11-02 | M2-06 | Control: orchestration tools (worker launch/stop for eligible tasks, runs list), per-worker git worktrees on the node, notifications to control for actionable events, bounded control restart with session resume and an urgent human action beyond the budget | this commit (`feat(control)`) | `TestPrepareWorktree`, `TestControlLaunchesWorkersInWorktrees`, `TestControlSupervisor` |

## 4. Process deviations

| ID | Date | Deviation | Reason | Follow-up |
|---|---|---|---|---|
| D-1 | 2026-11-02 | The root commit's message was written by hand following the `/cc` skill's bundled Conventional Commits spec. `/cc` did **not** produce it. | `/cc` runs `git log --oneline -10` during preprocessing, and that fails on a branch with no commits ("does not have any commits yet"), so the skill could not load. | Every later commit uses `/cc`. |
