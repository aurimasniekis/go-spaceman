# Contributing to SpaceMan

- Read [`AGENTS.md`](AGENTS.md) and [`docs/GOAL.md`](docs/GOAL.md) first. GOAL.md is the product contract.
- Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/). Add a `CHANGELOG.md` entry under `[Unreleased]`
  ([Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)).
- `make check` must pass. It runs `go vet`, the custom analyzers, golangci-lint, the traceability check, the analyzer
  tests, the coverage floor and the race tests. It doesn't run the UI checks or the drift checks: run `make web-check`
  when `web/` changes, `make api-check` when the API changes, `make sqlc-check` when the SQL changes, and `make site-check`
  when the site, `docs/` or the `Makefile` change. CI runs all of them. While iterating, use `make check-fast`
  ([AGENTS.md](AGENTS.md), "Gates").
- Security controls need negative tests. A change that relaxes security needs an ADR labeled **security-relevant change**.
- Forbidden: `math/rand` for anything security-related, unauthenticated encryption, network or file I/O inside a store write transaction,
  logging secrets, prompt text, terminal output or request bodies.
- Mocks and fakes belong in tests only. Never present one as an integration. When real-environment evidence is missing, record it in `docs/TRACEABILITY.md`.
- Opt-in real-harness tests (`SPACEMAN_REAL=1 make test-real`) use your local Claude Code and Codex logins with tight budgets, and an
  isolated herdr-core session. They never touch your default hc core.
