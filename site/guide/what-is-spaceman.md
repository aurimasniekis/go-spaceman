# What is SpaceMan?

SpaceMan is a self-hosted control plane for **persistent teams of coding agents**. It runs Claude Code and Codex inside
[herdr-core](https://github.com/herdrdev/herdr-core) terminals on machines you own, and gives the people responsible for those
agents one place to see what is running, watch it, stop it and audit it.

It has two programs:

- **`spaceman`** (central) holds the state: users and access, nodes, spaces, runs, leases, recordings and the audit log.
  It serves the HTTP API and the web UI.
- **`spaceman-agent`** runs on each node. It connects out to central over a WebSocket, launches harnesses in its own
  herdr-core core, records their terminals and enforces leases locally.

## What it does today

- **Spaces** pinned to a node, with goals and standing instructions, pause modes (hold, interrupt, emergency stop) and
  archive.
- **Runs** of Claude Code or Codex launched into a space, each in its own herdr-core terminal with an isolated harness
  configuration directory, a run-scoped credential and a minimal MCP bridge.
- **Execution leases** with fencing, disconnect enforcement and a lease-guard terminal that ends runs when the agent
  daemon is dead.
- **Recording** of every run's terminal, shipped to central, with a live watch-only view, replay and download.
- **Access control**: password and TOTP sign-in, step-up re-authentication, viewer/operator/admin roles per space or
  globally, scoped API tokens, the bootstrap root token and a hash-chained audit log.

## What is coming

The inbox and execution queue, Pulse event subscriptions (including GitHub), human actions and approvals, accounts and
secrets, prompts and profiles, the MCP gateway, memory and knowledge bases, backups, SSO through Porch, and
a Terraform provider are planned in later milestones. They are not described as features on this site until they work;
see the [project status](/guide/status).

## What it is not

SpaceMan assumes a trusted team. A space folder is not a sandbox, and anyone with a shell on a node can read that
node's spaces. The [known limitations](/reference/limitations) page lists the boundaries in full.
