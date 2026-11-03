<p align="center">
  <img src="site/public/logo.svg" alt="" width="72" height="72">
</p>

<h1 align="center">SpaceMan</h1>

<p align="center">
  <strong>A self-hosted control plane for persistent teams of coding agents.</strong><br>
  Claude Code and Codex run in herdr-core terminals on your own machines, under leases you control, with every terminal recorded and every action audited.
</p>

<p align="center">
  <a href="https://aurimasniekis.github.io/go-spaceman/">Documentation</a> ·
  <a href="https://aurimasniekis.github.io/go-spaceman/guide/try-locally">Try it locally</a> ·
  <a href="https://aurimasniekis.github.io/go-spaceman/guide/status">Project status</a> ·
  <a href="https://aurimasniekis.github.io/go-spaceman/security/">Security model</a>
</p>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="site/public/screenshots/landing-overview-dark.png">
  <img src="site/public/screenshots/landing-overview-light.png" alt="SpaceMan overview with three spaces, three active runs and two nodes">
</picture>

> **Status: early development.** Milestone 1 (central, nodes, spaces, runs, leases, recordings, access control and the
> web UI) is implemented and tested. The inbox, queue, events, human actions, accounts, prompts, MCP gateway, memory,
> backups, SSO and Terraform provider are planned. See the [project status](docs/TRACEABILITY.md).

## How it works

```mermaid
flowchart LR
  B["Browser / API"] -->|"HTTPS"| C["spaceman<br/>users · spaces · leases · recordings · audit"]
  A["spaceman-agent on each node"] -->|"outbound WebSocket"| C
  A --> H["herdr-core"]
  H --> R["Claude Code / Codex runs"]
```

- **`spaceman`** (central) keeps the state and serves the API and web UI.
- **`spaceman-agent`** runs on each node, dials central, launches harnesses in its own herdr-core core, records their
  terminals and enforces execution leases, even when it dies (a lease-guard terminal ends the runs).
- Restarts of either side never start a second copy of a run: runs are found again by their terminal labels.

## Screenshots

| Space with its lease and runs | Live, watch-only terminal |
|---|---|
| <img src="site/public/screenshots/space-overview-dark.png" alt="Payments platform space with node, lease and runs"> | <img src="site/public/screenshots/terminal-live-dark.png" alt="Live watch-only terminal of a control run"> |

Screenshots are the real UI on a demo stack with fictional data and a stub harness (`make screenshots`).

## Quick start

Requires Go, Node.js 24, pnpm and [herdr-core](https://github.com/herdrdev/herdr-core) (`hc`).

```sh
(cd web && pnpm install)
scripts/dev-stack.sh up            # prints a stack directory; source its env file
```

Then follow [Try it locally](https://aurimasniekis.github.io/go-spaceman/guide/try-locally) (also in
`site/guide/try-locally.md`). `make e2e` runs the whole first-run journey in a headless browser.

## Documentation

- Guides and operator docs: `site/` (VitePress), published at <https://aurimasniekis.github.io/go-spaceman/>
- Installation (Linux release archives, systemd, container images and which of them are agent-ready):
  `site/operators/install.md`, `site/operators/containers.md`, `site/operators/upgrade.md`
- Product contract: [docs/GOAL.md](docs/GOAL.md) · plan: [docs/PLAN.md](docs/PLAN.md) · decisions: [docs/DECISIONS.md](docs/DECISIONS.md)
- Known limitations: [docs/LIMITATIONS.md](docs/LIMITATIONS.md) · threat model: [docs/THREAT_MODEL.md](docs/THREAT_MODEL.md)
- Verified dependency contracts: [docs/contracts/](docs/contracts/)

## Security

SpaceMan assumes a trusted team and is not a sandbox. Read [docs/LIMITATIONS.md](docs/LIMITATIONS.md) before running it
anywhere that matters, and report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md).

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
