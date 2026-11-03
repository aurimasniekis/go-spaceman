# What is SpaceMan?

SpaceMan is a self-hosted control plane for **persistent teams of coding agents**. It runs Claude Code and Codex inside
[herdr-core](https://github.com/herdrdev/herdr-core) terminals on machines you own, and gives the people responsible for those
agents one place to see what is running, watch it, stop it and audit it.

It has two programs:

- **`spaceman`** (central) holds the state: users and access, nodes, spaces, runs, leases, recordings and the audit log.
  It serves the HTTP API and the web UI.
- **`spaceman-agent`** runs on each node. It connects out to central over a WebSocket, launches harnesses in its own
  herdr-core core, records their terminals and enforces leases locally.

## What it does

- **Spaces** pinned to a node, with goals and standing instructions, pause modes (hold, interrupt, emergency stop),
  archive and [relocation](/operators/relocation) to another node.
- **Runs** of Claude Code or Codex in their own herdr-core terminals, each with an isolated harness configuration
  directory, a one-run credential and an MCP bridge to SpaceMan's [agent tools](./agent-tools).
- **Execution leases** with fencing, disconnect enforcement and a lease guard that ends runs when the agent daemon dies.
- **Coordination**: a space's [inbox](./inbox) and [execution queue](./queue), a [control agent and workers](./control)
  with their own Git worktrees, durable [messages](./messages) and [schedules](./schedules).
- **People in charge**: [human actions](./human-actions) (questions, approvals, to-dos, custom types) across spaces, and
  [enforced approvals](./approvals) that gate operations SpaceMan performs.
- **Configuration**: [accounts](/operators/accounts) with encrypted credentials, versioned [prompts, profiles and
  roles](./roles-and-prompts), [skills](./skills) and an [MCP gateway](./mcp-gateway) to external tools.
- **Knowledge**: per-role [memory](./memory) preserved centrally, [knowledge bases](./knowledge) with support sessions,
  [dreams](./dreams) that consolidate memory, and [cross-space requests](./cross-space).
- **Events**: [Pulse subscriptions](./events) (including GitHub through go-pulse-github) into the inbox, and
  [SpaceMan's own events](./publishing-events) published through an outbox.
- **Recording and usage**: every terminal recorded with live view and replay; [usage and transcripts](./usage).
- **Access and operations**: passwords with TOTP, [single sign-on](./sign-in) through Porch or another OIDC provider,
  roles per space, scoped API tokens, a hash-chained audit log, [limits and budgets](./limits),
  [backups](/operators/backup), metrics, a [command line](/operators/cli) and a [Terraform provider](/operators/terraform).

## How finished it is

Everything above is implemented and covered by automated tests, most of them **simulated** (fakes or embedded
dependencies). Some of it has also been run against the real dependency: herdr-core, Claude Code, Porch, Pulse with
go-pulse-github, MinIO and systemd in a container. Codex, AWS S3, a physical Linux host and amd64 under systemd have
not. No version has been released. The [project status](/guide/status) lists the evidence for every requirement, and
the [known limitations](/reference/limitations) list what is missing.

## What it is not

SpaceMan assumes a trusted team. A space folder is not a sandbox, and anyone with a shell on a node can read that
node's spaces. The [known limitations](/reference/limitations) page lists the boundaries in full.
