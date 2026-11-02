# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed
- A space's goals and standing instructions now reach every run's system prompt (the launch dialog already said so).
- Accessibility (WCAG 2.1 AA): contrast of dimmed text, accent colors and badges in both themes; named close buttons,
  account menu, replay slider and task-list checkboxes; space sections are navigation links; the docs site's search
  button, links and code colors.

### Added
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
- Documentation site (VitePress with Mermaid), real screenshots captured by `make screenshots`, a local development stack
  (`scripts/dev-stack.sh`) and a browser end-to-end journey (`make e2e`).
- Product contract (`docs/GOAL.md`), architectural decisions, execution plan, requirement traceability matrix and verified dependency
  contracts for herdr-core, Pulse, go-pulse-github, Porch, Hallpass, Claude Code and Codex.
