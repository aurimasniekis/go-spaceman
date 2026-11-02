# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Documentation site (VitePress with Mermaid), real screenshots captured by `make screenshots`, a local development stack
  (`scripts/dev-stack.sh`) and a browser end-to-end journey (`make e2e`).
- Product contract (`docs/GOAL.md`), architectural decisions, execution plan, requirement traceability matrix and verified dependency
  contracts for herdr-core, Pulse, go-pulse-github, Porch, Hallpass, Claude Code and Codex.
