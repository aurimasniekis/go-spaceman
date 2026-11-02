# Third-party components and attribution

Release builds generate the full dependency list as SPDX SBOMs (`make sbom`). This file records components whose licence or
attribution needs specific mention.

| Component | Use | Licence | Notes |
|---|---|---|---|
| herdr-core (`hc`) | Runner, invoked as an external binary | Apache-2.0 | Not vendored. Version recorded in node capability inventory. |
| go-pulse | Event bus client | Apache-2.0 | Go module dependency. |
| go-pulse-github | Event contract reference for seed templates | MIT | Contract facts only; fixtures used in tests are cited by path and revision. |
| Night Owl colour theme (Sarah Drasner) | UI and site palette inspiration | MIT | Palette values adapted; attribution kept here. |
| ghostty-web 0.4.0 | Browser terminal for live and replayed runs | MIT | npm dependency. It inlines Ghostty's VT parser as WebAssembly (ADR-0020). |
| uqr 0.1.3 | TOTP enrollment QR codes, rendered locally | MIT | npm dependency. |
| Pulse web UI (go-pulse) | `web/src/api/session.ts`, `components/QRCode.tsx`, `e2e/totp.ts`, `postcss.config.cjs`, `tsconfig.json`, the app shell layout | Apache-2.0 | Same author. Adapted, not vendored. |
| go-pulse v0.1.3 SDK (`pkg/pulse`) | Event consumption (durable subscriptions) | Apache-2.0 | Go module dependency, pinned at the inspected commit `9aba53b`. |
| go-pulse-github v0.1.0 fixture | `internal/events/template/testdata/pulse-github-issues-opened.json` wraps `testdata/webhooks/issues_opened.json` | MIT | Derivation documented in that folder's README. |
| nats-server v2.15.1, nats.go, nats jwt, nkeys | In-process NATS server for tests (`internal/events/pulsetest`), NATS client | Apache-2.0 | The server is linked only into test binaries and the `devpulse` development tool, never into `spaceman` or `spaceman-agent`. |
| MCP Go SDK v1.8.0 (`github.com/modelcontextprotocol/go-sdk`) | The agent's stdio MCP server (`spaceman-agent mcp-bridge`) | Apache-2.0 and MIT (licence transition stated in its LICENSE) | Go module dependency. |
| jsonschema-go v0.4.3 (`github.com/google/jsonschema-go`) | JSON Schemas of agent tool inputs | MIT | Go module dependency. |
| @axe-core/playwright 4.14.0 | Accessibility audits of the UI and the site (tests only) | MPL-2.0 | npm dev dependency; not shipped. |
| VitePress 1.6.4 `VPNavBarSearchButton.vue` | `site/.vitepress/theme/components/VPNavBarSearchButton.vue` (shortcut hints hidden from assistive technology) | MIT | Adapted copy; styles unchanged. |
| go-hallpass HallPass skill (`integrations/skills/hallpass` at `79a3c09`) | `internal/skills/seed/hallpass`, the built-in `hallpass` skill | Apache-2.0 | Same author. Copied unchanged; refreshed only by hand. |
| santhosh-tekuri/jsonschema v6.0.3 | Event template input schemas (JSON Schema 2020-12) | Apache-2.0 | External references are disabled. |
