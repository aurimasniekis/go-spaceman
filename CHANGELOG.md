# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Security
- Revocation reaches open streams at once (SM-AUTH-007, ADR-0033): signing out, revoking a session or API token,
  rotating the root token, disabling a user, resetting a password or changing grants ends affected live terminals and
  change-feed streams as soon as the change commits. A removed space grant hides that space's changes immediately and
  ends its terminals with "Access lost". Expiry, idle timeout and changes made while central was stopped are caught by
  a periodic re-check (`auth.stream_revalidate`, default 30 s), which now also re-checks node connections' credentials.
  The web UI returns to sign-in when its session is gone and reconnects when it was only rotated.

### Changed
- `auth.stream_revalidate` accepts at most 60 s (was 5 min).
- Launches can now be refused with `rate_limited`: a space runs at most `limits.max_concurrent_runs` worker and diagnostic
  runs (default 8), and the node's reported `capacity` is enforced. Control, KB support and dreamer runs have their own pools.
- KB support sessions can no longer ask knowledge bases (support depth 1 by default), so support never starts more support.
- Event deliveries record their origin (live, redelivery, replay, reprocess) and attempt; replays are recorded in a replay
  history and items they create are marked.

### Fixed
- Live updates: a browser whose change-feed position is ahead of the server (after a restore from backup) now gets a gap
  and reloads instead of silently skipping changes.
- A space's goals and standing instructions now reach every run's system prompt (the launch dialog already said so).
- Accessibility (WCAG 2.1 AA): contrast of dimmed text, accent colors and badges in both themes; named close buttons,
  account menu, replay slider and task-list checkboxes; space sections are navigation links; the docs site's search
  button, links and code colors.

### Added
- Limits and budgets (SM-LIMIT-001/002, ADR-0036): one limits model with installation defaults (`limits:`), per-space
  overrides and a single effective read with sources (API and the space's **Limits** page). Concurrency, tool calls per
  run, event ingestion per subscription, KB support sessions, pending human actions, storage, task deadlines and
  usage-based token and cost budgets. Lowering a limit never stops running work unless the space chooses `on_lower: stop`.
  Budgets warn and stop new runs; they are not a hard currency limit, because usage is reported late.
- Schedules (SM-LIMIT-003): inbox items or tasks at cron or interval occurrences in an IANA time zone, with skip, run-once
  or bounded catch-up for missed runs and per-occurrence deduplication, driven by the single deadline timer (the space's
  **Schedules** page and API).
- Explicit replay of a subscription with a reason (`POST …/subscriptions/{id}/replay`), its history, and an option to
  create fresh items (SM-EVT-015).
- `spaceman` central: SQLite storage with migrations, keyring, hash-chained audit log, metrics and health endpoints, and the
  administrative HTTP API (OpenAPI 3.1, RFC 9457 problems, `If-Match`, idempotency keys, cursor pagination, live change feed).
- Access control: root-token bootstrap, Argon2id passwords, mandatory TOTP for privileged accounts, recovery codes,
  step-up re-authentication, password reset tokens, break-glass recovery with an acknowledgement alert, sessions with CSRF
  protection, viewer/operator/admin roles per space or globally (Casbin), and scoped API tokens.
- Nodes: one-time enrollment, node credentials, an outbound WebSocket protocol (`spaceman.node/v1`) with capability
  inventory, execution leases with generation fencing, disconnect enforcement and a lease-guard terminal for dead daemons.
- `spaceman-agent`: herdr-core runner over the framed protocol, idempotent launch and adoption, restart reconciliation,
  Claude Code and Codex adapters with isolated configuration directories, a launcher shim that keeps secrets out of
  herdr-core state, run credentials and a minimal MCP bridge.
- Spaces pinned to a node with goals, standing instructions, pause modes (hold, interrupt, emergency) and archive.
- Terminal recording into zstd segments, shipped to central, with a live watch-only stream, replay and download.
- Web UI (Mantine, Night Owl and Light Owl themes): setup, sign-in, overview, spaces, runs with live and replayed
  terminals, nodes, users, API tokens, audit log and account pages.
- Attachments: files uploaded to a space with sniffed types, size and quota limits, safe serving and garbage collection.
- Inbox per space: operator items in Markdown or JSON with attachments, urgency, statuses (in progress, deferred, rejected,
  done), deduplication by source key, and a TipTap editor that accepts pasted, dropped or attached images and files.
- Pulse events: connections with encrypted credentials, a catalog of versioned event templates (built-in GitHub issue and
  pull request templates), space subscriptions with one durable consumer each, explicit cursors (start now, replay),
  delivery history, quarantine with reprocessing, and template testing with sample events.
- Execution queue per space: one explicit order (priority on arrival, operators after), incident policy, dependencies with
  cycle detection, explicit blockers, exclusive resources, an active-task limit, selection with recorded skip reasons, and
  drag or keyboard reordering. Inbox items can become tasks.
- Human actions: questions, approvals (advisory for now), to-dos and messages for people, separate from the inbox and the
  queue, with type-specific responses, claiming, comments, withdrawal, expiry and follow-ups. An action can block its
  task until someone responds. A "Needs you" page lists pending actions across the spaces you can read.
- Accounts: Claude subscription tokens, Anthropic and OpenAI API keys stored sealed and never shown again, allowed per
  space with a default per harness. Launches pick the account deterministically and refuse clearly when none fits;
  rotation applies to later launches.
- Agent tools: every run's SpaceMan MCP server now serves the tools its kind may use, over the node connection: reading
  and adding inbox items, reading and managing tasks, fetching attachments, and asking people through human actions.
- Messages: durable messages and requests between agents, control and operators, with delivery states SpaceMan tracks.
  Idle agents get a one-line notice in their terminal; content is read through their tools. Human responses reach the
  agent that asked the same way. Each space has a Messages tab.
- Control and workers: control starts and stops workers for eligible tasks through its tools, each writer in its own git
  worktree (never reset or deleted). Control is notified of new inbox items, tasks awaiting review and ended workers,
  and is restarted with its session resumed if it stops unexpectedly, at most three times an hour.
- Roles and prompts: versioned prompts (drafts, publish, extend, fork, diff, rollback), harness profiles, role templates
  and space roles. Launches resolve them in a documented order with a permission-mode ceiling, and each run records its
  configuration in a manifest.
- Usage and transcripts: runs report token usage read from the harness's own records (Claude Code transcripts), with
  the harness's cost estimate and its assumptions; unknown usage is shown as unknown. Ended runs upload their native
  transcript, which can be downloaded or deleted.
- Dreams: a dreamer agent consolidates role memory from new transcript segments on demand or on a schedule, within
  time and evidence budgets; it reports findings, and the evidence it read counts as done only after its memory
  changes are checkpointed. A built-in prompt describes the method.
- Knowledge bases: agents ask a space's KB repositories through their tools; each requester gets its own support
  session (queued at capacity, put to sleep when idle, recovered when it fails); answers cite evidence and say whether
  they rest on accepted knowledge; proposals are accepted on the node after the KB's check, never forced.
- Role memory: each role's notes are Git history on the node, shared by its instances, checkpointed at idle points and
  run ends, merged without overwriting (conflicts are preserved and reported) and preserved centrally by fast-forward
  only. Spaces have a Memory section.
- Enforced approvals: policies make built-in agent tools or gateway tools wait for a person's approval, bound to the
  exact arguments, the requesting run, an expiry and a number of uses; the decision and each execution are recorded
  separately, and the approver's authorization is checked again before execution.
- MCP gateway: administrators register external MCP servers with sealed headers and grant them to space roles. Runs
  see their tools as `<server>__<tool>`, each run with its own upstream session; calls are rechecked, bounded, audited
  and dialed only to addresses the egress policy allows.
- Skills: a central catalog of versioned skills (SKILL.md plus reference files), including a built-in HallPass skill,
  assigned through role templates and space roles and written into each Claude Code run's config folder.
- Custom human-action types: administrators define request and response schemas, outcomes and form hints; spaces enable
  them; people answer through generated forms (or JSON) and agents discover them through their tools.
- Single sign-on: OpenID Connect providers such as Porch, with PKCE (S256), validated ID tokens, sealed write-only
  client secrets and refused private-network providers unless allowed. Group mappings (including Porch's
  `porch.groups`) grant a role in every space or one space, re-applied at every sign-in and only to SSO sessions.
  Pre-provisioned or just-in-time users, identity linking from the Account page, by an administrator or with a link
  invitation, step-up at the provider (or with the SpaceMan TOTP code where allowed), test logins, and a Sign-in
  providers page.
- Publishing SpaceMan's own events to Pulse (off by default, `events.outbound`): run, task, inbox item, human action,
  approval, memory and knowledge-base lifecycle events with versioned, metadata-only payloads, written in the same
  transaction as the change and relayed after commit with stable IDs, retries with backoff, dead-lettering, retention,
  correlation, causation and origin headers. SpaceMan never re-ingests its own events. Publication status (connection,
  backlog, lag, failures) on the Events page for administrators, in the API and as Prometheus metrics.
- Backups and restore: sealed backups (every file encrypted under the KEK, which is never included) of the database,
  role memory (Git bundles), recordings, attachments, transcripts and the effective configuration, with a manifest
  stating what is included and excluded, per-file SHA-256 and a sealed copy of the manifest. Local or S3-compatible
  targets, a schedule with one timer, retention by count and age, verification that opens every file and checks the
  database copy and audit chain, a Backups page, `/api/v1/system/backups` and `spaceman backup create|list|verify|restore`.
  A restore comes up with every space paused and execution authority renewed (lease generations advanced, earlier runs
  fenced, commands cancelled, run credentials revoked), event subscriptions replaying from their oldest stored delivery,
  approvals with uses left revoked and pending ones kept, and a restore notice until the spaces are resumed.
- Human-action follow-up: policies per action type with space overrides (Needs you shows reminded and escalated
  actions; **Policies** on a space's Human actions tab, installation defaults on Action types) for reminders, escalation
  (raise the priority, assign someone; the order never changes), rate limits per agent run and per space, and what
  happens to a response when the requester's run has ended: rerouted at once to a running successor-role run, else to
  control, else shown as undeliverable to people (with a message to operators and **Send to control**), or kept for the
  next run of the same agent instance until a hold ends. Routing is durable across restarts and records the original
  run, the previous target and the reason; responses are never shown to unrelated runs. Expiry, reminders, escalation,
  holds and approval expiry share one timer for the earliest deadline (the once-a-minute expiry job is gone). New
  outbound events `human.action.reminded`, `escalated` and `rerouted`; API under `/api/v1/human-policies`,
  `/api/v1/spaces/{id}/human-policies` and `…/notifications/{id}/route`.
- Packaging (Linux):
  - Reproducible release archives for linux/amd64 and linux/arm64 (`make release-snapshot`). Each holds `spaceman` with
    the UI embedded, `spaceman-agent`, systemd units, example configs and the licence files. They come with SPDX SBOMs
    (when syft is installed) and `SHA256SUMS`.
  - A hardened `spaceman.service`, and a `spaceman-agent.service` whose relaxations for harnesses are explained in
    ADR-0034.
  - A central container image (Debian slim with git), a node agent base image that is labelled and documented as
    **not agent-ready** (no herdr-core, no harnesses), and an example for building an agent runtime image from
    operator-supplied binaries.
  - A central-only Compose file, a container smoke test (`make image-smoke`), and a tag-triggered release workflow
    that only drafts a GitHub release and pushes no images.
  - Installation, containers (with the image matrix) and upgrade pages.
- Terraform provider (`terraform-provider-spaceman/`, phase 1, not published): 20 resources for spaces, space roles and
  account bindings, accounts, harness profiles, role templates, prompts, skills, MCP servers and grants, event
  connections, templates and subscriptions, human-action types and their space enablement, approval policies, knowledge
  bases, dream configurations, OIDC providers and group mappings, plus node, space and event-template data sources.
  Credentials are write-only arguments with a version to rotate them; updates send `If-Match` with the version from
  private state; catalog changes publish exactly one immutable version; UI edits show as drift only for configured fields;
  a `node_id` change is refused until relocation exists. Acceptance tests run against a throwaway local central
  (`make tf-acc`).
- Documentation site (VitePress with Mermaid), real screenshots captured by `make screenshots`, a local development stack
  (`scripts/dev-stack.sh`) and a browser end-to-end journey (`make e2e`).
- Product contract (`docs/GOAL.md`), architectural decisions, execution plan, requirement traceability matrix and verified dependency
  contracts for herdr-core, Pulse, go-pulse-github, Porch, Hallpass, Claude Code and Codex.
