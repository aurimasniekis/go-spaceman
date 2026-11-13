# Building and testing

## Requirements

Go (the version in `go.mod`), Node.js 24, pnpm, and for the end-to-end and real suites
[herdr-core](https://github.com/herdrdev/herdr-core) (`hc`) and the Playwright Chromium browser
(`cd web && pnpm exec playwright install chromium`).

## Everyday checks

| Command | What it runs |
|---|---|
| `make check-fast` | The inner loop: `go vet`, the custom analyzers, golangci-lint, the traceability check, the analyzer tests and every Go test without the race detector, from go test's cache (only what changed runs again). `make sqlc-check` and `make api-check` run when their inputs changed since the merge base with `origin/main`. |
| `make check` | The gate to run once before pushing: `go vet`, the custom analyzers, golangci-lint, the requirement traceability check, the analyzer tests, the coverage floor (one `go test -cover` run over the security-critical packages) and the race-enabled tests, in that order, stopping at the first failure |
| `make web-check` | UI typecheck, unit tests (vitest) and the production build |
| `make api-check` | Regenerates `api/openapi.json` and the UI's TypeScript types and fails on drift |
| `make site-check` | Builds this site (dead links, screenshot references), parses every Mermaid diagram, checks the API, CLI and configuration references for drift and audits every page with axe. The audit runs only when something the site is built from changed against `main` (`scripts/site-affected.sh`, the same rule CI uses); `SM_SITE_A11Y=1` runs it regardless. `make site-verify` runs all of it except the audit; `make site-a11y` runs only the audit, on the built site, split over `SM_A11Y_SHARDS` parallel Chromium workers (default: a quarter of the CPUs, 1 to 8). |
| `make fuzz-smoke` | Every `Fuzz*` target for `FUZZTIME` (30 s by default) |

## What CI runs

| Workflow | When | What |
|---|---|---|
| `ci` | every pull request and push to `main` | lint, vet, sqlc drift, analyzer tests and traceability; race tests; the coverage floor; the login timing comparison (`make test-login-timing`, on its own runner); the web checks and API drift; `make site-verify`; govulncheck and gitleaks |
| `ci` | pull requests that change `site/`, `docs/`, `web/`, `api/`, `CHANGELOG.md`, the `Makefile`, `ci.yaml` or `scripts/site-affected.sh` (the rule lives in that script), and every push to `main` | the site's axe audit (`make site-a11y`, two shards) |
| `ci` | nightly | all of the above with the full audit, and every fuzz target for 10 minutes |
| `release` | `v*` tags and manual runs | vet, race tests, the coverage floor, the login timing comparison, a 10 s fuzz smoke and the full `make site-check`; no archive or image is built until they pass |

Store-backed tests fail when a write transaction holds the write lock for longer than 100 ms (Hallpass HP-DB-004), or
the value of `SM_TEST_TX_MAX`. Time spent waiting for the writer and retrying a busy database doesn't count
(ADR-0070). GitHub's hosted runners are slower, so the coverage and race steps in `ci` and `release` use 1 s, and
the login-timing and fuzz jobs 250 ms (ADR-0062).

A store opened by `storetest.Open` is set up so that setup work doesn't land inside a test's transactions (ADR-0062,
update 2026-11-13). `storetest.Seed` flushes its copy of the database to disk before the store opens, because
otherwise the first commit flushes it. The store has 2 reader connections (`storetest.Readers`; `WithReaders` sets
another size) instead of the production `max(4, GOMAXPROCS)`, because each one parses the whole schema when it opens.
`make race` builds modernc.org's SQLite without the race detector's `checkptr` instrumentation. A transaction that
still trips the guard is usually not doing too much work: it is waiting for a CPU or for a commit's `fsync`. To check,
run the package under `taskset -c 0` with `TMPDIR` on a real disk, not tmpfs.

The wall-clock comparison of failed logins for known and unknown accounts (Hallpass HP-AUTH-005) needs a quiet
machine without the race detector or coverage, so it runs only through `make test-login-timing`, which CI and the
release workflow run as their own job. The deterministic checks (one Argon2id verification against a dummy hash with
the same parameters, and the same database reads for both accounts) run in every suite (ADR-0071).

Locally, `make race` uses go test's cache, so it reruns only the packages whose code, dependencies or test inputs
changed. CI and the release workflow run `make race RACE_COUNT=1`, which runs every test (ADR-0070). To rerun a
suspected flake, run its package with `-count=1`.

### Heavy gates on a shared machine

`make race`, `make coverage`, `make site-a11y`, `make e2e` and `make screenshots` take a slot of a machine-wide
semaphore first (`scripts/gate-slot.sh`, `flock` lock files), so several people's or agents' full suites on one
machine take turns instead of slowing each other down. `make check` takes one slot for the coverage floor and the race
tests together. A run that has to wait prints a `waiting for a heavy-gate slot` line naming the holders.
`SM_GATE_SLOTS` sets the number of slots (2 on 16 or more CPUs, otherwise 1; `0` turns it off, and it's off in CI) and
`SM_GATE_LOCK_DIR` the shared directory (`~/.cache/spaceman-gates` by default). A slot is released when its holder
exits, even when it's killed.

`make race` runs `internal/app`, the slowest package, as its own `go test -race` next to the others, with its own
`-parallel` cap `RACE_APP_PARALLEL` (`min(16, nproc/2)`, at least 4; CI uses 4); the other packages keep
`RACE_PARALLEL` (4). On 16 or more CPUs, internal/app's serial tests run in a second process at the same time
(`RACE_APP_SPLIT`, `0` turns it off).

### When a release gate fails

- **`write transaction "…" held the write lock for … (> 1s, Hallpass HP-DB-004)` in the coverage step:** rerun the failed job once
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
