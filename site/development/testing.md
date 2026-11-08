# Building and testing

## Requirements

Go (the version in `go.mod`), Node.js 24, pnpm, and for the end-to-end and real suites
[herdr-core](https://github.com/herdrdev/herdr-core) (`hc`) and the Playwright Chromium browser
(`cd web && pnpm exec playwright install chromium`).

## Everyday checks

| Command | What it runs |
|---|---|
| `make check` | `go vet`, the custom analyzers, golangci-lint, race-enabled tests, coverage floors and the requirement traceability check |
| `make web-check` | UI typecheck, unit tests (vitest) and the production build |
| `make api-check` | Regenerates `api/openapi.json` and the UI's TypeScript types and fails on drift |
| `make site-check` | Builds this site (dead links, screenshot references), parses every Mermaid diagram, checks the API, CLI and configuration references for drift and audits every page with axe. `make site-verify` runs all of it except the audit; `make site-a11y` runs only the audit, on the built site. |
| `make fuzz-smoke` | Every `Fuzz*` target for `FUZZTIME` (30 s by default) |

## What CI runs

| Workflow | When | What |
|---|---|---|
| `ci` | every pull request and push to `main` | lint, vet, sqlc drift, analyzer tests and traceability; race tests; the coverage floor; the web checks and API drift; `make site-verify`; govulncheck and gitleaks |
| `ci` | pull requests that change `site/`, `docs/`, `web/`, `api/`, `CHANGELOG.md`, the `Makefile` or `ci.yaml`, and every push to `main` | the site's axe audit (`make site-a11y`) |
| `ci` | nightly | all of the above with the full audit, and every fuzz target for 10 minutes |
| `release` | `v*` tags and manual runs | vet, race tests, the coverage floor, a 10 s fuzz smoke and the full `make site-check`; no archive or image is built until they pass |

Store-backed tests fail when a write transaction takes longer than 100 ms (Hallpass HP-DB-004), or the value of
`SM_TEST_TX_MAX`. GitHub's hosted runners are slower, so the coverage steps in `ci` and `release` use 250 ms; the race
steps use 1 s (ADR-0062).

### When a release gate fails

- **`write transaction "…" took … (> 250ms, Hallpass HP-DB-004)` in the coverage step:** rerun the failed job once
  (`gh run rerun <run-id> --failed`). Hosted runners have noisy neighbours. If it fails again, or the same transaction
  shows up in several runs, treat it as a regression: run `make coverage` locally, where the limit is 100 ms, and move
  the slow work out of the transaction. Don't raise `SM_TEST_TX_MAX` to get a release through.
- **Fuzz smoke:** the log names the target and the failure. The input file it mentions stays on the runner, so fuzz
  that target locally (`go test -run '^$' -fuzz '^<FuzzName>$' ./<package>`) until it fails. Fix the code and commit the
  input that `go test` writes under `testdata/fuzz/<FuzzName>/` as a regression case.
- **Site check:** the log names the page, the color scheme and the axe rule. Run `make site-check` locally.

## End-to-end and real suites

| Command | What it runs |
|---|---|
| `make e2e` | A fresh local stack (`scripts/dev-stack.sh`): real central with the embedded UI, a real `spaceman-agent` with its own herdr-core core and a stub harness. Playwright drives setup, sign-in with TOTP, node enrollment, a space, a live and replayed run and a password reset. |
| `make test-real` | Go tests tagged `real` against an isolated herdr-core core: the framed protocol, launch and adoption, the lease guard after `SIGKILL`, agent and central restarts, and disconnect enforcement. It never touches your default core. |
| `make test-real-codex` | A real, billed Codex turn through SpaceMan, with `OPENAI_API_KEY` or `SPACEMAN_REAL_CODEX_AUTH_FILE` (the `auth.json` of a dedicated ChatGPT login). |
| `make test-harbor-pilot` | A real Harbor Pilot gateway (`harbor-pilot serve --demo` v0.1.3, the minimum version SpaceMan supports) on loopback, with its own test binding, budget, service principal and parent token. It uses `HARBOR_PILOT_BIN`, or installs the pinned release with `go install`. The upstreams are synthetic, so no provider credentials are needed. |
| `make test-real-harbor-pilot` | Real Claude Code and Codex runs through SpaceMan (real central, node agent, herdr-core and launcher shim) and the same real Harbor Pilot gateway, with no provider credentials. It checks the harness's environment, Harbor Pilot's record of each request (binding, trusted references, session), that no token is stored at rest, and that the run's token is refused after the run. Codex comes from `SPACEMAN_REAL_CODEX_BIN` or `PATH`. |
| `make test-real-s3-external` | The artifact store and an S3 backup against your own bucket (AWS S3, Cloudflare R2, a self-hosted service), under a random prefix it deletes afterwards. |
| `make test-fault` | The fault-injection suite (`test/fault`): a full disk while recording, an interrupted recording and upload, the agent killed mid-run, central `SIGKILL`ed mid-write (the audit chain must verify after restart), a disconnect beyond the lease, a network partition during a command, corrupted node state and database contention. In process, with the in-memory runner. |
| `make test-pulse` | The Pulse/NATS integration tests against an embedded nats-server with the real go-pulse SDK: ingestion, deduplication, fan-out, quarantine, replay, the loop guard and outbox publication |
| `make demo` | The scripted end-to-end demonstration ([details](/development/demo)): thirty steps against a real central, two real agents with their own herdr-core cores, devpulse and devmcp, through the browser, the API and the CLI. Stub harness by default; `SPACEMAN_DEMO_REAL=1` makes the Claude Code worker real. |
| `make screenshots` | Regenerates the documentation screenshots ([details](/development/screenshots)) and audits every captured page with axe (WCAG 2.1 A/AA) in both themes; any violation fails the run |
| `make site-check` | Builds the site, checks Mermaid diagrams and API reference drift, and audits every site page with axe in both color schemes |

Simulated tests use an in-memory runner that models the herdr-core behaviour SpaceMan depends on; the `real` suite checks
that the model matches the real program. `docs/TEST_STRATEGY.md` in the repository maps every verification topic to its
simulated and real tests. The traceability matrix on the [status page](/guide/status) keeps the two
kinds of evidence apart.

## Building

| Command | Output |
|---|---|
| `make build` | `bin/spaceman`, `bin/spaceman-agent` (API only) |
| `make build-ui` | The same with the web UI embedded |
| `make site` | The static site in `site/.vitepress/dist` |
