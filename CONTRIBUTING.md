# Contributing to SpaceMan

- Read [`AGENTS.md`](AGENTS.md) and [`docs/GOAL.md`](docs/GOAL.md) first. GOAL.md is the product contract.
- Commits follow [Conventional Commits 1.0.0](https://www.conventionalcommits.org/). Add a `CHANGELOG.md` entry under `[Unreleased]`
  ([Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/)).
- `make check` must pass. It runs lint, vet, race tests, web lint/typecheck/test and the traceability and API drift checks.
- Security controls need negative tests. A change that relaxes security needs an ADR labeled **security-relevant change**.
- Forbidden: `math/rand` for anything security-related, unauthenticated encryption, network or file I/O inside a store write transaction,
  logging secrets, prompt text, terminal output or request bodies.
- Mocks and fakes belong in tests only. Never present one as an integration. When real-environment evidence is missing, record it in `docs/TRACEABILITY.md`.
- Opt-in real-harness tests (`SPACEMAN_REAL=1 make test-real`) use your local Claude Code and Codex logins with tight budgets, and an
  isolated herdr-core session. They never touch your default hc core.
