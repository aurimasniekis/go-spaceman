# End-to-end demonstration

> Requirement: SM-VER-003 (the end-to-end demonstration in the brief's §41 is scripted and reproducible, and its evidence
> is recorded). Driver: `test/demo` (build tag `demo`), browser steps `web/e2e/demo.spec.ts`, entry point `make demo`
> (`scripts/demo.sh`). Test strategy: [TEST_STRATEGY.md](TEST_STRATEGY.md). Requirement evidence: [TRACEABILITY.md](TRACEABILITY.md).

## What runs

One Go test, `TestDemo`, builds everything from the working tree and runs the thirty steps of §41 in order against a
fresh installation under one short `/tmp/smdemo-*` directory:

- a real `spaceman serve` with the embedded UI (development mode, its own data directory and KEK, a root token from
  `spaceman admin bootstrap`), on free random loopback ports;
- two real `spaceman-agent` processes (`demo-node-a`, `demo-node-b`), enrolled with one-time tokens, each with its own
  herdr-core session, socket and XDG directories. The default herdr-core core is never used: `HC_TERMINAL_ID`,
  `HC_TERMINAL_SOCKET`, `HC_SCOPE` and every other `HC_`, `XDG_`, `SPACEMAN_` and harness-credential variable are removed
  from every process the driver starts;
- devpulse (`internal/events/pulsetest/devpulse`): a NATS server in operator/JWT mode with JetStream, published to with
  the real go-pulse SDK as the `pulse-github` publisher. It is Pulse-shaped, not a `pulsed` deployment;
- devmcp (`internal/gateway/gatewaytest/devmcp`): a fake ticket tracker behind SpaceMan's MCP gateway, reachable only with
  a bearer token that SpaceMan holds;
- the stub harness (`internal/harness/harnesstest/stubharness`) as the program of both harness kinds, unless real mode
  replaces the Claude Code binary (below).

The driver acts as the operator through three surfaces: the browser (Playwright in Chromium, steps 1 and 16), the HTTP
API with the root token, and the `spaceman` command line (task ordering, `task next`, `human respond`). Agents act only
through their own MCP bridge (`spaceman-agent mcp-bridge`), with their own run credentials, like real harnesses.

At the end it stops everything it started (agents, central, devpulse, devmcp, both herdr-core cores), fails the run if
any process started from its directory is still alive, and removes the directory (`SM_KEEP=1` keeps it, with the logs
and the browser's screenshots). It never runs `rm` with wildcards.

## Running it

```sh
make demo                          # stub harness for both kinds; no credentials, reproducible
SM_KEEP=1 make demo                # keep /tmp/smdemo-* for inspection
```

Prerequisites: Go, Node.js 24, pnpm, Git, herdr-core (`hc`) and the Playwright Chromium browser
(`cd web && pnpm exec playwright install chromium`). A run takes about three minutes after the build. It does not use `make e2e`'s fixed
ports, so it does not collide with a development stack, but it builds `web/dist` (`SM_DEMO_SKIP_UI_BUILD=1` reuses it).

Real mode makes the Claude Code worker of step 5 a real Claude Code run. Everything else stays as in stub mode: control,
the Codex worker, KB sessions and the dreamer run as the Codex kind, whose program is still the stub. The token is read
inside the command and never printed or written to the repository:

```sh
TOK=$(zsh -lic 'printf %s "$CLAUDE_CODE_OAUTH_TOKEN"' 2>/dev/null)
SPACEMAN_DEMO_REAL=1 SPACEMAN_DEMO_CLAUDE_TOKEN="$TOK" make demo
unset TOK
```

The run is budget-bounded: model `haiku`, one short prompt that asks for two SpaceMan tool calls and no file edits or
commands, and the run is stopped as soon as its task reaches review (or after four minutes). The credential exists only
in the driver's memory and in a SpaceMan account. Step 5 checks that it is not stored in clear anywhere under the demo
directory (central's data, the logs, node run folders, herdr-core state). No Codex credential is available, so Codex is
never exercised for real.

## Evidence

Every step appends one JSON line to the evidence file: the step number and title, `pass` or `fail`, the evidence kind,
what was checked (with the IDs of what it created), notes, the failure message, the start time and duration, and the
mode. The default file is `/tmp/spaceman-demo-<mode>-<time>.jsonl`, outside the repository and outside the run
directory, so it survives the cleanup (`SPACEMAN_DEMO_EVIDENCE=FILE` chooses another). The driver prints a summary table
at the end. Evidence lines hold IDs, states and counts, never credentials, prompts, transcripts or terminal output.

Evidence kinds:

- **real**: real processes only (central, the agents, herdr-core, Git, NATS with the real go-pulse SDK, a browser). No
  harness behaviour is involved, or the step only checks what SpaceMan did around a program.
- **simulated**: what is shown depends on the stub harness standing in for Claude Code or Codex. It makes the tool calls
  a real agent would make through the same bridge, tools and authorization. Everything around it is real.
- **mixed**: real mode only. A real Claude Code run takes part alongside stub programs.

## The steps

| # | Step (§41) | How it is shown | Evidence |
|---|---|---|---|
| 0 | Stack | Builds from the working tree, starts devpulse, devmcp, central (bootstrap, root token) and two enrolled agents with their own cores, and creates the administrator (TOTP required) | real |
| 1 | UI creation of a pinned space | Browser: TOTP sign-in, New space form with Pinned node `demo-node-a`, waits until the UI shows the lease as valid. API: node and lease. Audit: `space.created` by the user, not the root token | real (browser) |
| 2 | Accounts, repositories and KB configured | Claude and Codex accounts (secrets never echoed) bound as defaults; spaces `docs` and `ops` (`disconnect_policy: kill`); repositories in the space folders on the node (one pushed to a bare origin, the KB repository without a remote, the docs repository pushed); KB `payments` (review policy, three sessions) with a kb role; MCP server with a grant and an approval policy; a custom human-action type; queue policy | real |
| 3 | Event template with a repository variable | Connection to devpulse (credentials never echoed); built-in `github-issue` template; preview with the verified go-pulse-github fixture; the same template version subscribed in two spaces with `repository=example-org/payments-api`; a `*` variable refused | real |
| 4 | Verified source event becomes a durable inbox item | An issue published with the publisher's own NATS credentials is accepted into an inbox item with subscription provenance, a dedup key and SpaceMan-set trusted metadata; an issue of another repository on the same subject is filtered. Step 21 reads the item again after a central restart | real (devpulse, not `pulsed`) |
| 5 | Control delegates to Claude Code and Codex | The event becomes task A; control (a Codex-kind run) launches a Claude Code worker for A and a Codex worker for B through `spaceman_worker_launch`; both run, are assigned, and were created by `agent:<control>` | simulated (stub mode); mixed (real mode: the Claude Code worker is real) |
| 6 | Separate worktrees | Each task has its own `worktrees/spaceman-<task>` on branch `spaceman/<task>`; `git worktree list` shows both; herdr-core reports each worker's terminal in its own worktree | real (Git, herdr-core); stub programs |
| 7 | Independent KB sessions while another is busy | Three requesters (control and both workers) ask the KB; the KB sessions take 12 s per request, and the driver observes two sessions each busy with a different requester's request at the same instant; one session per requesting instance, each a `kb` run on its own branch | simulated |
| 8 | Correlated KB answers with evidence | Every request is answered with basis `committed` and evidence `README.md` at the KB's `main` commit; each requester gets a notification whose `correlation_id` is its request | simulated |
| 9 | Role-memory central sync | A worker writes `decisions.md` and a note in `$SPACEMAN_MEMORY_DIR`; at its end the node checkpoints, central acknowledges the head and serves the file from its own copy; a new instance of the role starts with the file | simulated (writer); real (checkpoint, sync, sharing) |
| 10 | Operator ordering | Priority insertion, then the operator moves a low-priority task to the top and another after it with the CLI; `spaceman task next` follows the operator | real |
| 11 | Blocked top item skipped | An external blocker on the top task: `queue/next` selects the next eligible task and records the skipped one with its reason in the decision history | real |
| 12 | Incident policy and manual demotion | Policy `incident_insertion: top`; an alert (custom subscription with an input schema) becomes an incident task at the top; the operator drags it to the bottom; the same alert republished (Pulse deduplicates it) and explicitly replayed (SpaceMan records `duplicate`, no new item) leaves it there; escalation raises its priority without moving it | real |
| 13 | A worker question and a human to-do | The Codex worker asks a question that blocks task B and a to-do; the task carries the human-action blocker; the to-do is completed with `spaceman human respond` | simulated (requester); real (CLI) |
| 14 | Gated operation approval | Control's `tickets__ticket_comment` is held: a pending approval bound to the run and an argument digest, with a human action, no execution; control's recording shows it waiting | simulated |
| 15 | Custom-schema human action | The worker's `deploy-window` action keeps type version 1; a request and a response that break the schemas are refused (422); a valid response with outcome `scheduled` is stored | simulated (requester); real (validation) |
| 16 | Global operator response | Browser: the question is answered from the global Needs you page; audit records the user | real (browser) |
| 17 | Durable reply delivered to the requester | The response's notification targets the requesting run; it moves from pending to delivered to acknowledged when the worker fetches it through its bridge; the worker's recording shows the answer | simulated |
| 18 | Other work continues | While control's approval is still pending, the Codex worker goes on after the answer and moves task B to review, the Claude Code worker moves task A to review, runs keep running and the queue still offers work. Real mode stops the real run here and records its usage | simulated (stub mode); mixed (real mode) |
| 19 | Approval and execution shown separately | The approval is decided through the API with the root token (approved, by whom, when, uses left) with no execution yet; the execution is a separate record (succeeded) made when control calls again; exactly one execution | simulated |
| 20 | Dreamer reads local files, traceable results | The node prepares the dream workspace (DREAM.md, memory checkouts, transcript evidence copied from the Claude Code worker's session files on the node); the dreamer reads them, edits memory and reports findings citing the evidence files; the memory change is synced; audit `dream.reported` by the dream's run | simulated (dreamer); real (workspace, evidence, sync) |
| 21 | Restarts do not duplicate execution | Agent restart (SIGTERM) and central restart (SIGTERM, same data directory): every live run keeps its terminal, herdr-core holds exactly one terminal per run, no run is added, the event's inbox item survives | real |
| 22 | Disconnect behaviour | Central stopped beyond lease plus grace (8 s + 6 s): runs are still live after 3 s; then the node ends every run on its own (kill for `ops`, interrupt for `payments`); when central returns the ends are reported and nothing is relaunched | real |
| 23 | Interrupted recording readable | The agent is SIGKILLed mid-run and the driver cuts the last five bytes off the newest recording segment (an injected torn write); the local recording still reads; the restarted agent adopts the run and central's copy continues and is readable through the API | real (fault injected by the driver) |
| 24 | Clean relocation | `docs` to `demo-node-b`: a precheck with no issues, pause, relocation done, the space on the destination and still paused, a new lease generation, the repository at the source's commit, the source folder retained; resumed, a new run starts on node B | real (two agents on one machine; a local bare repository as the remote) |
| 25 | Dirty relocation refused | Uncommitted work in a worktree and a repository without a remote: the relocation stops at the check (`blocked`, with issues), nothing moves or appears on node B, and after cancelling the work is untouched | real |
| 26 | Restore: paused, no stale authority | A run is live in `ops`; a backup is made and verified through the API; central is stopped and `spaceman backup restore` restores into a new data directory; central comes up with a restore notice, every space paused, a new lease generation, the old run fenced and stopped, launches refused until the operator acknowledges and resumes | real |
| 27 | Duplicate events do not duplicate items | After the restore the subscription re-reads its stream (duplicates recorded); the same event republished with the same ID; an explicit replay: every redelivery is `duplicate`, the space still has one item per event | real |
| 28 | Two subscribed spaces both receive | A new issue reaches `payments` and `docs` as two inbox items through two durable consumers (both spaces paused after the restore, still ingesting) | real |
| 29 | Website shows real screenshots | `site/scripts/check-shots.mjs`; every `current` row of [SCREENSHOTS.md](SCREENSHOTS.md) exists as a 1440×900 PNG in both themes (captured from the real UI by `make screenshots`); rows that are not current are listed, not claimed | real |
| 30 | Mermaid renders | `pnpm check:mermaid` in `site/`: every Mermaid block in `docs/`, `site/` and `README.md` parsed by the real Mermaid parser in headless Chromium | real |

## What the demonstration does not show

- No real Codex run: there is no Codex credential. The Codex kind is always the stub.
- In stub mode no model is contacted. Steps marked simulated show SpaceMan's behaviour around an agent that makes the
  expected tool calls, not that a model makes them.
- devpulse is NATS with the real go-pulse SDK, not a `pulsed` deployment or real GitHub webhooks.
- Relocation runs between two agents on one machine, with a local bare repository as the remote.
- The restore uses the local backup target, not S3.
- Step 23's torn write is injected by the driver after a real `SIGKILL`; the fault suite (`make test-fault`) covers the
  disk-full and upload interruption cases.

## Recorded runs

| Date (UTC) | Mode | Commit | Result | Notes |
|---|---|---|---|---|
| 2026-11-03 | stub | `c01a1cd` plus the uncommitted demonstration changes | pass: steps 0–30 all pass (31 of 31), 138 s including the build | macOS (Darwin 24.6, arm64), Go 1.27.3, herdr-core 0.1.16, Chromium (Playwright). Evidence kinds: real for 0–4, 6, 10–12, 16, 21–30; simulated for 5, 7–9, 13–15, 17–20. Step 7 saw three KB sessions busy at once; step 22 ended all seven runs 13.5 s after central stopped (kill for `ops`, interrupt for `payments`); step 26 moved the lease generation from 1 to 1000002 and the run from before the backup was stopped by its node. No process was left behind. |
| 2026-11-03 | real (attempted) | same | not run | The Claude Code credential could not be read: the agent session ran in an isolated worktree whose sandbox refuses the `zsh -lic` command that reads `CLAUDE_CODE_OAUTH_TOKEN`, and the token is not in that environment. `scripts/demo.sh` stopped before starting anything (exit 2). No real Claude Code evidence from this demonstration yet. Codex: never exercised for real (no credential). |
| 2026-11-03 | real | `3baa1d9` | pass: steps 0–30 all pass (31 of 31) | Run from the operator's shell with the token read inside the command and never printed. Step 5 is mixed: control and the Codex worker are stub programs, the Claude Code worker is the real `claude` (model haiku) with the account's token; it used SpaceMan's tools and moved task A to review (step 18), and was then stopped (budget bound). The credential was not found in clear anywhere under the demo directory. The run's usage was recorded as unknown (`source=unavailable`): the run was stopped before its transcript yielded usage. Codex: still never exercised for real. |
