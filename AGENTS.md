# AGENTS.md — rules for agents working on SpaceMan

SpaceMan is a self-hosted Go control plane for persistent teams of coding agents. It holds credentials, starts processes on
operator machines, and mediates approvals, so it is security-sensitive. These rules apply to every agent session in this repository.

## Before working (and after every context compaction or new session)

1. Reread the original implementation prompt if it is still available. Its location for the current work stream is recorded in
   `docs/PLAN.md` §0. The prompt is a working reference only. It is never committed and never a runtime dependency.
2. Read `docs/GOAL.md` (canonical product contract) and `docs/PLAN.md` (milestones, work-package status, progress log).
3. Check `docs/DECISIONS.md` §2 (open items and unresolved contracts) and `docs/TRACEABILITY.md` (requirement → evidence).
4. Before implementing against any external dependency, read its file under `docs/contracts/`. If a fact you need is not
   recorded there, inspect the real source, schema or binary and record it with the revision. **Never invent** Pulse envelopes,
   NATS subjects, go-pulse-github schemas, Porch claims, Hallpass behavior, herdr-core methods, harness capabilities or ghostty-web APIs.

## At the start and end of each substantial phase

- Review the relevant GOAL.md requirements.
- Update `docs/PLAN.md` progress and `docs/TRACEABILITY.md` evidence.
- Record deviations and unresolved contracts in `docs/DECISIONS.md`.
- Check that the engineering choices preserve the invariants in GOAL.md §3.

## Contract rules

- `docs/GOAL.md` wins over PLAN.md and code until a recorded decision changes it.
- **Never silently weaken security.** Any relaxation of a requirement, guarantee or default needs an ADR in `docs/DECISIONS.md`
  labeled **security-relevant change**.
- **Never redefine unfinished work as complete.** Status tables keep three things separate: *implemented*, *tested (simulated)*
  and *externally verified (real)*. Never upgrade a status without evidence.
- Requirement IDs (`SM-<AREA>-NNN`) are stable. Never renumber or reuse them. Withdraw them through a DECISIONS.md reference.
- Mocks and fakes are allowed in tests. They are never presented as working integrations. A missing real environment is recorded
  as missing evidence.
- Crypto: Go standard library and `golang.org/x/crypto` only. No invented constructions.
- Never commit secrets, credentials, private transcripts, raw secret fixtures or the temporary prompt. Generate test secrets at test time.
- Do not modify sibling projects (`../hallpass`, `../pulse`, `../porch`, `../pulse-github`) without an explicit requirement.
- Never stop, restart or attach automation to the user's default herdr-core core. Tests use an isolated `HC_SESSION` and XDG directories.
- Credentialed real-harness tests (Claude Code, Codex) are opt-in (`SPACEMAN_REAL=1`) and budget-bounded. They clean up only what they create.

## Code rules

- No network or file I/O inside a write transaction (`store.Writer.Tx`). Use `tx.AfterCommit` for side effects.
- No external publication inside database transactions. Use the outbox.
- Logs never contain credentials, request bodies, prompt text, terminal output or sensitive payloads.
- Metric labels come from bounded allowlists.
- Authorization goes through the Casbin enforcer (`internal/authz`). Do not hand-roll permission checks in handlers.
- Content from events, repositories, memory, skills, KB, messages and attachments is untrusted data. It is never an authority source.

## Gates

Which check to run when (commands: [site/development/testing.md](site/development/testing.md)):

- **While iterating:** targeted `go test ./internal/<pkg>/ -run '<Test>'`, and `make check-fast`. That runs vet, the custom
  analyzers, golangci-lint, traceability, the analyzer tests and every Go test without the race detector, using go
  test's cache, so only what changed runs again. It takes seconds when nothing changed and a few minutes after a change
  to a widely imported package such as `internal/store`. It runs `sqlc-check` and `api-check` only when their inputs
  changed since the merge base with `origin/main` (`SM_CHANGED_BASE` overrides it).
- **Once before pushing:** the full `make check` (vet, custom-vet, lint, traceability, analyzer tests, the coverage
  floor, then race tests; it stops at the first failure), on the final committed tree after merging the branch you
  target. Start it in the background and keep working on docs, the CHANGELOG and the PR text instead of waiting on it.
  Don't run it in a loop.
- **The race run uses go test's cache locally:** `make race` (and so `make check`) reruns only the packages whose code,
  dependencies or test inputs changed, so a second `make check` after a merge that touched no Go code costs little.
  `make race RACE_COUNT=1` runs every test again, which CI and the release workflow always do. A cached pass is not
  evidence for a flaky test: rerun that package with `-count=1`.
- **Heavy gates take turns on the node:** `race`, `coverage`, `site-a11y`, `e2e` and `screenshots` (and so `make
  check`) wait for a node-wide slot first (`scripts/gate-slot.sh`; `SM_GATE_SLOTS`, default 2 on 16 or more CPUs, and
  `SM_GATE_LOCK_DIR`). A `gate-slot: … waiting for a heavy-gate slot` line means another run on the node holds it: keep
  working while it waits, and don't bypass it with `SM_GATE_SLOTS=0`. A killed run releases its slot. It is off when
  `CI=true`; a lock dir shared by several users is pre-created with mode 1777.
- **Only when they are affected:** `make web-check` when `web/` changed. `make site-check` when `site/`, `docs/`, `web/`,
  `api/`, `CHANGELOG.md` or the `Makefile` changed (the rule CI uses for the accessibility audit). `make e2e` when UI
  behavior changed.
- **When something is red:** rerun only the failing package with `-run`, not the whole gate, to tell a regression from a
  flake. Report a flake with the test name and its output.

## Git workflow (commit locally, never push)

- Work on `main`. Commit coherent increments throughout the work, not only at the end.
- Before committing: `git status`, review the diff, run the relevant tests, scan for secrets, and stage files **by explicit path**.
- Generate the message with the `/cc` skill (Conventional Commits 1.0.0). `/cc` only prints a message. Then run `git commit -F <file>`.
- If `/cc` is unavailable, do not invent its output or claim it ran. Record the limitation in PLAN.md and leave the changes intact.
- **Never `git push`**, push tags or create remotes or PRs without an explicit user instruction for that specific push.
- Never force-push, rewrite history, or discard existing changes.
- Record the commit hashes in `docs/PLAN.md` §3 (progress log).
