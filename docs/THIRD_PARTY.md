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
