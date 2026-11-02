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
| M1-03 | API: huma, problem errors, idempotency, If-Match, pagination, change feed + SSE watch; management listener | SM-API-002, SM-OBS-001/002 | todo |
| M1-04 | Auth: root-token bootstrap, first admin, Argon2id login, TOTP, sessions, CSRF, throttle, API tokens, Casbin | SM-AUTH-001/003/004/005/006 | todo |
| M1-05 | Node protocol: enrollment, connect, hello/capabilities, heartbeat, leases, fencing, node journal | SM-NODE-001…007 | todo |
| M1-06 | hc runner: contract spikes K-1/K-2/K-3, client, idempotent launch, adoption, reconcile | SM-RUN-001/002, SM-NODE-004/008/009 | todo |
| M1-07 | Harness adapters: Claude Code + Codex, isolated dirs, launch shim, MCP bridge (minimal tools) | SM-HARN-001/002/003, SM-ACCT-004 | todo |
| M1-08 | Spaces: CRUD, pinning, pause modes, archive/restore/purge; launch a run | SM-SPACE-001…004 | todo |
| M1-09 | Recording: zstd segments, upload, live view, replay | SM-REC-001…004 | todo |
| M1-10 | Restart/disconnect reconciliation tests (central, node, hc) | SM-NODE-005/007/009 | todo |
| M1-11 | Web shell: Night Owl theme, bootstrap/sign-in, spaces, nodes, overview, terminal | SM-UI-001/002/004 | todo |
| M1-12 | Site skeleton, Mermaid, demo seed, Playwright screenshots, coverage inventory | SM-DOCS-001…005 | todo |

### M2 — Coordination core
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M2-01 | Attachments + TipTap composer | SM-ATT-* | todo |
| M2-02 | Inbox (operator text/JSON/attachments, sources, statuses) | SM-INBOX-* | todo |
| M2-03 | Pulse ingestion: connections, durable consumers, dedup, quarantine, fan-out, metrics | SM-EVT-011…016 | todo |
| M2-04 | Event templates + subscriptions + GitHub presets + UI preview/testing | SM-EVT-001…010, 017 | todo |
| M2-05 | Execution queue: ordering, dependencies, exclusions, eligibility, incidents, demotion | SM-QUEUE-* | todo |
| M2-06 | Control + workers + worktrees + durable messages + delivery adapters | SM-CTRL-*, SM-COMM-*, SM-REPO-* | todo |
| M2-07 | Human actions: default types, lifecycle, MCP, reply routing; per-space + global UI | SM-HUM-*, SM-HUI-* | todo |
| M2-08 | Docs + screenshots for M2 | SM-DOCS | todo |

### M3 — Configuration, intelligence, memory
| WP | Scope | Requirements | Status |
|---|---|---|---|
| M3-01 | Accounts + encrypted secrets + resolution | SM-ACCT-* | todo |
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
| 2026-11-02 | M1-01 | Planning: prompt saved, contracts inspected, tracking docs created | root commit (see §4 D-1) | gitleaks: no leaks |

## 4. Process deviations

| ID | Date | Deviation | Reason | Follow-up |
|---|---|---|---|---|
| D-1 | 2026-11-02 | The root commit's message was written by hand following the `/cc` skill's bundled Conventional Commits spec. `/cc` did **not** produce it. | `/cc` runs `git log --oneline -10` during preprocessing, and that fails on a branch with no commits ("does not have any commits yet"), so the skill could not load. | Every later commit uses `/cc`. |
