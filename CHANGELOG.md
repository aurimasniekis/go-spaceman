# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Harbor Pilot run tokens on central (ADR-0060, Proposed; HP-T3). Nodes deliver them to harnesses from HP-T4 on;
  until then a gateway run's harness gets no model credentials, so don't enable Harbor Pilot for a space yet.
  - A harness a space routes through Harbor Pilot uses no account: a launch that names one is refused, a model outside
    the space's allowed aliases is refused, and there is never a fallback to a direct account. A launch is refused
    before it starts when no connection is configured. The run's manifest records the gateway and project.
  - Each run gets its own child token when its launch is dispatched, bound to the space, run, instance, session (for a
    fresh Claude Code gateway run, central chooses the session ID) and project, with the space's lifetime. A re-sent launch
    reuses it; a token that would expire within 5 minutes is replaced. A mint Harbor Pilot refuses fails the run as
    `gateway_unavailable` with the reason (for example "parent token lacks tokens:mint"); network failures, 5xx and
    429 are retried three times first.
  - Every end path queues the token's revocation in its own transaction: the run's reported end, a launch that can't be
    sent, a stop or pause, a relocation, a purge, and runs a node left unknown past its lease. Central revokes them on
    its deadline timer, retrying with backoff until the token expires; queued revocations survive a restart.
    Replacing the parent token revokes live run tokens with the old parent first (the new one can't).
- Shipped event templates for Harbor Pilot events (HP-T6, ADR-0060 §7): `harbor-pilot-budget` (budget warnings and
  exhausted budgets; `project_id` sends a project's budgets to its space), `harbor-pilot-quota` (provider quota windows
  that ran out or recovered) and `harbor-pilot-access-blocked` (requests Harbor Pilot refused by policy, including
  blocking-budget refusals; `budget_only` keeps just those). Optional filters narrow by budget, account, monitor or
  binding. Titles and correlation keys hold only Harbor Pilot's ids, names, enums and amounts, checked by each
  template's input schema, which also pins the severity, times and counts the body shows, so a forged value cannot add
  Markdown structure; refusal reasons, model names and quota labels appear only in the body (as code) and the raw payload.
  Events without a project are not routed anywhere automatically: a space gets them only by subscribing (Q4 is open).
- Harbor Pilot connection and per-space settings (ADR-0060, Proposed; HP-T2). Runs don't use the gateway yet (HP-T3/T4).
  - An administrator connects one Harbor Pilot gateway (Integrations → Harbor Pilot, `/api/v1/harbor-pilot/connection`,
    account:write; changing or removing it needs a step-up): a gateway URL, an optional URL runs use, a parent token
    with `tokens:mint` and an optional service-principal token. Both tokens are sealed and write-only: answers, the
    audit log and the change feed show only whether each is set and a version counter.
  - Stored tokens are only sent to the address they were entered for: changing the gateway URL or the URL runs use, or
    newly allowing plain HTTP, needs the tokens again in the same request ("re-enter the tokens when the gateway URL
    changes", 422, before any call). Central's client uses no proxy and never dials link-local, unspecified or
    multicast addresses (private and loopback ones stay allowed).
  - Every save is checked with Harbor Pilot: a probe token is minted and revoked at once, which shows the parent can
    mint ("parent token lacks tokens:mint" otherwise), which client binding it belongs to and when it expires; a gateway
    older than Harbor Pilot 0.1.3 is refused. A service-principal token must read budget status and projects.
  - A space opts in on its overview (`/api/v1/spaces/{id}/harbor-pilot`, space_config:write): its Harbor Pilot
    project (checked with Harbor Pilot; setting or changing it needs account:write, since it decides who pays, and
    an update without a project keeps the stored one), the harnesses routed through it (any harness; default Claude Code and Codex),
    the run-token lifetime (24 h by default, 15 minutes to 7 days) and what a launch does when budget status can't be
    read (proceed or refuse).
  - The connection can't be removed while a space uses it or unexpired tokens (run or validation) are live or waiting
    to be revoked. Migration 0040 (forward-only) adds the connection, space
    settings, run-token and revocation-queue tables.

### Changed
- Harbor Pilot groundwork (still no product behaviour) moves to Harbor Pilot v0.1.3, now the minimum version: the
  contract, `make test-harbor-pilot` (installs and checks v0.1.3) and ADR-0060 (Proposed). The test harness also checks
  per-run usage by trusted reference, budget refusals and the budget check on run-token mint. Claude Code's default
  model now works through the gateway (K-13 resolved on the demo gateway).
- Claude Code runs start with its classic renderer (`"tui": "default"` in the run's settings) unless the profile's
  Claude Code settings set `tui`. Its full-screen renderer, which Claude Code otherwise often picks itself, keeps earlier output
  out of the terminal, so a watcher could not scroll back. Set `"tui": "fullscreen"` in a profile to keep it (ADR-0064).
  The node applies this default, so it takes nodes on this release or later; runs on older nodes keep Claude Code's
  own choice, and the profile's resolved settings in the API don't show it.
- Tests: most tests run in parallel; `make race` caps per-package parallelism with `RACE_PARALLEL` (default 4).

### Fixed
- A relocation whose source was unavailable marked the source's runs lost without ending them: no `run_ended` event,
  no end count, and nothing that reacts to a run's end (recordings, KB sessions, control) learnt of it. They now
  end like runs a node reports ended.
- Central's Harbor Pilot client also refuses "this network", broadcast and AWS's IPv6 metadata address, and link-local
  addresses reached through NAT64 or 6to4. A project check's probe token that couldn't be revoked is reported as a
  warning instead of being dropped silently.
- Run terminals scroll back without a takeover (operator request). The wheel, and a drag on a phone, scroll through
  earlier output in the live view without sending anything to the run. While you read, new output no longer pulls you
  to the bottom, and **Jump to live** returns.
- During a takeover, the wheel no longer types arrow keys into a full-screen program, which made Claude Code show
  "Scroll wheel is sending arrow keys". It sends mouse-wheel reports when the program asks for mouse input (ghostty-web
  0.4.0 turned every wheel step in an alternate screen into arrow keys).
- The run terminal no longer flickers between two font sizes on some window widths (the font fit now measures the
  canvas instead of predicting it).
- Tests: `internal/app` TestFleetLifecycle no longer fails at random with "timed out waiting for runs running after
  central restart". Reopening the store checks every reader connection, one per CPU by default, and under the race
  detector on a 32-CPU host that took 5-7 s, longer than the 5 s lease plus 1 s grace the test then had. The agent then
  stopped the runs, as it should without central (SM-NODE-007), so they never returned to running. The fleet test
  central now uses 4 readers (new test-only `app.Options.StoreReaders`), which brings a restart to about 1 s; restart
  tests also get a 30 s lease as a margin under load, and the test fails at once, with the cause, if a restart takes
  5 s or more. Product behaviour is unchanged.
- A person's response to a human action whose requester had ended (for example control's own question, answered after
  control stopped) became undeliverable when no control was running: control was not auto-started for it, and a
  control started later was not told about it. With control auto-start on, it now goes to control, starts control,
  and the control (or one launched by hand later) is told once about each unacknowledged response (ADR-0056 note).
- The "New inbox item" dialog focuses the item text when it opens, instead of the title.

## [0.1.0-rc.9] - 2026-11-07

Ninth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): Claude Code settings on
harness profiles (ADR-0059) with their review fixes; faster CI and `make check`, with the release workflow gated on
vet, race tests, coverage, fuzz smoke and the full site check (ADR-0062); a fix for the queue page crashing on
duplicate task titles; more built-in GitHub event templates and optional template filters (ADR-0061); live updates
that reconnect after central restarts; terminal and mobile UI fixes, with form descriptions under their fields;
relocation of spaces whose repositories use SpaceMan SSH keys; and the Harbor Pilot gateway contract with ADR-0060
(Proposed; documents and an opt-in test harness, no product behaviour). Upgrade central and the node agents. The
relocation fix needs both: an older agent keeps working, but relocating a space with keyed repositories to it still
fails. The relocation fix changes no node protocol, but Claude settings on profiles move it to 1.8: a run of a profile
with `claude_settings` on an older agent fails as `node_too_old`. One migration, 0039 (`claude_settings_json` on
harness profiles), which is forward-only and cannot be reversed: take a backup before upgrading (central also writes a
pre-migration snapshot; going back means restoring it). The new event templates are seeded at central's start, not by
a migration.

### Added
- More built-in GitHub event templates (ADR-0061), seeded at central's start on new and existing installations:
  - `github-pr-comment` and `github-issue-comment`: new comments on pull requests and on issues;
  - `github-pr-review`: submitted reviews (approved, changes requested, commented);
  - `github-pr-review-comment`: new inline comments on a pull request's diff;
  - `github-workflow-run` and `github-check-suite`: completed GitHub Actions runs and check suites, by default only
    failed, timed out, cancelled or startup failures (`include_successes` takes every conclusion).

  Each takes the account and optional `repository`, `owner` and `exclude_author` (for a space's own agent account).
  Comment and review templates ignore bots unless `include_bots` is set; CI templates can drop them with
  `exclude_bots`. Comments and reviews correlate with their pull request or issue, CI with its commit. Item titles
  hold no free text from the event: titles, comments and workflow names are only in the body and the payload. A
  built-in whose slug one of your templates already uses is not seeded; central logs a warning naming the slug.
- Template filters can be optional: `if_set` applies a filter only when a variable has a value, `unless_set` only
  when it has none. A new operator `absent` matches a missing or null field.
- Claude Code settings on harness profiles (ADR-0059, security-relevant): a claude-code profile's `claude_settings` are
  settings.json keys, any keys (permissions, auto mode rules, env, hooks, …). They are set in each run's own
  `settings.json` at launch, and keys taken out of the profile are removed at the next launch. The profile form edits
  permissions, auto mode rules (with a switch to keep or replace Claude Code's defaults) and environment variables,
  and other keys as JSON. `permissions.defaultMode` is held to the permission mode ceiling. Nodes need this release
  (protocol minor 8); a run of such a profile on an older node fails as `node_too_old` instead of starting without
  them. Migration 0039.
- Groundwork for routing run model traffic through a Harbor Pilot gateway (no product behaviour yet):
  - a contract pinned to Harbor Pilot v0.1.1 (`docs/contracts/harbor-pilot.md`);
  - ADR-0060, **Proposed** and security-relevant, with the operator's answers to its design questions recorded (one, routing of unscoped events, still open);
  - requirements SM-GW-001…010;
  - an opt-in test harness that runs a real `harbor-pilot serve --demo` (`make test-harbor-pilot`, `SPACEMAN_HARBOR_PILOT=1`); the gateway stops with the test binary even when its cleanup never runs, and a cached binary is reinstalled unless it is the pinned build;
  - a recorded spike: Claude Code 2.1.317 and interactive codex-cli 0.166.0 both reach the gateway with only a
    per-run token in their secret environment. Codex needs no login seeding.
- `SM_CHROMIUM_ARGS`: extra Chromium launch arguments (space-separated) for `make e2e`, `make screenshots` and
  `make site-check`, for hosts whose sandbox stops Chromium from starting (for example
  `--no-sandbox --disable-gpu --no-zygote` in a container). Unset by default and in CI.

### Changed
- Form field descriptions are shown under the field instead of between the label and the field, so fields in one row
  line up whether or not they have a description. The validation error comes after the description. One theme
  default (`inputWrapperOrder`) applies it to every form.
- On a phone the app bar shows only the logo, not the "SpaceMan" name, so it no longer stacks over a page header with
  the same name.
- Faster CI and `make check`:
  - test stores and test centrals start from a database that was migrated once per test binary, instead of applying
    every migration again for each test (about 10 s per store under the race detector);
  - CI runs lint and vet, race tests and the coverage floor as parallel jobs, each with its own Go cache;
  - `make fuzz-smoke` lists fuzz targets only in packages that declare one, and fails when listing them fails or
    finds none.
- CI scope (ADR-0062):
  - the coverage step allows 250 ms per write transaction on GitHub's hosted runners; locally it stays 100 ms;
  - on pull requests the site's accessibility audit runs only when site, docs, web, API or changelog files change;
    the site build, links, Mermaid and reference drift still run every time (`make site-verify`);
  - fuzzing runs nightly and in the release workflow, not on pull requests.
- The release workflow builds nothing until vet, race tests, the coverage floor, fuzz smoke and the full site check
  with its accessibility audit pass.

### Fixed
- Relocating a space whose repositories use SpaceMan SSH keys no longer fails in Prepare with "Host key verification
  failed" on a destination without Git credentials of its own. The destination clones such a repository from its
  configured URL with its key and pinned host keys, as a normal clone does. Central matches each relocated repository
  to the configured one (by folder, else by remote URL) and puts the key only on the message to the destination
  agent: stored commands, logs, the journal and the audit never hold it. Repositories without a key clone with the
  node's own Git authentication as before. An older destination agent ignores the key (no protocol change). A
  relocation also no longer drops the user name of an `ssh://git@host/…` remote URL.
- Terminals: the agent tab strip no longer scrolls vertically or shows scrollbars, and a tab's first line (status
  dot and name) no longer scrolls out of view on a phone. Tabs keep their content's size; when there are more than fit,
  the strip scrolls sideways (touch, trackpad or mouse wheel), snapping to tabs, and keeps the selected tab in view.
- Run header: the turn number and the state no longer get clipped ("#." / "working ·…"); the role name truncates
  first and the actions wrap to a second row when there is no room. On a phone the header is two rows: who (status,
  role, turn, state) and the actions (Live/Replay, then Take over, Stop and the details toggle as icon buttons).
- Terminals on a phone: the page no longer scrolls sideways (a terminal wider than the screen scrolls inside its own
  box), the run inspector goes under the terminal instead of beside it, the replay controls wrap, and the Terminals
  workspace's space and agent list is a drawer opened from the space name (a long name ends in an ellipsis).
- Live updates no longer stay "offline" after central restarts. A change feed that the browser gave up on (a 502
  while central restarts, a 401) or that the server ended is reopened with backoff (1 s doubling to 30 s, with jitter,
  reset once it opens): each try re-checks the session first, sending the browser to sign-in when the session is gone,
  and resumes from the last change seen. A stream the server ends as unauthenticated while the session is still valid
  reconnects at once the first time; when fresh streams keep ending that way, each further reconnect waits for the
  next backoff step. The header shows "offline" while waiting and "connecting" while trying. The
  tab holding the stream retries at once when the window comes back online, when it becomes visible again, and when
  another tab opens or reloads, so a stale background tab can't keep every tab offline. The tabs' lock and channel are
  now `spaceman-watch.v2`, so tabs still running an older build don't hold the new ones offline.
- The queue page no longer crashes ("Duplicate options are not supported") when two tasks share a title. Task pickers
  (a task's dependencies, a new task's parent and dependencies) use task IDs as values; a title shared by several
  tasks shows each task's status and the end of its ID. The run launcher's "another role" choice no longer collides
  with a space role named `custom`. The queue route has its own error boundary, and its task drawer and dialogs are
  contained separately, so a failing part shows an alert with a retry instead of blanking the page.
- Saving a harness profile without `claude_settings` keeps the stored settings, read in the save's own transaction; a
  failed read before the save could clear them.
- Changes to a profile's `claude_settings` are audited on `profile.created` and `profile.updated` with the changed
  top-level keys and a sha256 of the new settings, never the values.
- The node records the settings.json keys it owns before writing settings.json. When that record is corrupt, it moves
  `settings.json` and the record aside (`.corrupt-<unix nanoseconds>`), logs a warning naming both, and starts from fresh
  settings: no key an earlier profile set survives, and the instance recovers without a fix on the node.
- A run that can't be dispatched (`node_too_old`, `account_unavailable`) is now failed and its launch cancelled; ending
  it used to fail on a database constraint (a NULL session ID), which left the run pending. Its end is handled like a
  reported one: counted in `spaceman_runs_ended_total` (reason `launch_failed`), published once as run_ended, and a
  draining relocation of its space goes on. Failing an already-ended run only cancels its commands.
- Tests: `internal/events` TestReplayIsExplicitAndDistinctFromRetry, TestDurableIngestMetrics and other ingestion tests
  no longer read deliveries or metrics once right after waiting for the inbox item. The item is committed before its
  delivery is recorded and the metric after that, so the read could come too early (seen in CI as an index out of
  range panic and "durable created = 0"). Failure messages no longer index possibly empty lists.
- Tests: `internal/keyring` TestLoadKEKSources and `internal/cli` TestTokenSources pass under a restrictive umask (such
  as 077). They now chmod their deliberately world-readable files instead of relying on the mode given to WriteFile,
  which the umask narrows. Product code is unchanged.

### Security
- Profile settings may set endpoint and credential `env` variables, helpers and hooks: recorded as an accepted risk
  (ADR-0059, THREAT_MODEL T-32).

## [0.1.0-rc.8] - 2026-11-06

Eighth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): shared Git SSH keys, so one
key on a Git account clones many private repositories, and a clearer host-key error for repositories without a key.
Shared keys work with node agents from 0.1.0-rc.6 on (node protocol 1.7). The clearer error needs this release's agent.

### Added
- Shared Git SSH keys (ADR-0058, security-relevant). An administrator generates one key, adds its public half to a Git
  account (preferably a read-only machine user), and picks the spaces that may use it. Those spaces' repositories
  choose it instead of their own deploy key.
  - API: `/api/v1/git-ssh-keys`, and `/spaces/{id}/git-ssh-keys`.
  - CLI: `spaceman git-key`, and `spaceman repo update --git-key`.
  - UI: a Git SSH keys page, and a Shared Git SSH key choice on each repository.
  - Migration 0038.

### Fixed
- A host-key failure on a repository without a SpaceMan SSH key no longer talks about pinned known_hosts. Those pins
  apply only to keyed repositories. The message now says the node's own SSH setup does not know the host, and how to
  fix it: an `https://` URL for a public repository, a SpaceMan SSH key, or the node user's known_hosts.

## [0.1.0-rc.7] - 2026-11-06

Seventh release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): the first pass of the UX
review (Terminals views, one space layout, dark-theme status colours) and a security update of klauspost/compress. Only
central changes; node agents from 0.1.0-rc.6 keep working.

### Security
- `github.com/klauspost/compress` 1.20.2 fixes GO-2026-6865, which govulncheck reports as reachable through the zstd
  encoder used for terminal recordings.

### Added
- Watching agents (UX review §4):
  - A space's **Terminals** tab shows every running agent as a tab: a status dot, `role #turn` and its state in words.
    The selected agent's terminal is below. Runs that ended in the last day are under **Ended**.
  - The **Terminals** workspace (sidebar) lists every space with its agents next to the selected space's tabs, with a
    status bar of running agents and those waiting for input. Space and agent are in the URL.
  - `[` and `]` switch between agents.

### Changed
- Every space page, its run pages, inbox items and subscriptions share one layout. The space's header (name, state, node,
  Launch run, Pause, and a ⋯ menu for edit, export, relocate and the lifecycle) and its section tabs stay put, and a
  section has a one-line title. The run page is in the layout, with a "Terminals / control #4" breadcrumb, so you can go
  from a terminal to any section of its space.
- A run's page has a one-line header (status dot, role, kind and harness, turn, state, Live/Replay, Take over, Stop) and
  an inspector beside the terminal:
  - Now: state, turn, duration, human actions it waits on;
  - Task;
  - Usage: tokens and estimate;
  - Placement: node, account, worktree;
  - Launch and configuration, and Identifiers, both collapsed.
  The nine identifier cards are gone, an ended run shows a one-line summary, and the takeover toast is gone.
- The space overview leads with its running agents (with links to their terminals) and puts the run history at the
  bottom. The lease card is gone, and an invalid lease shows as an alert.
- In the dark theme, dimmed text is slightly lighter, so it stays readable on hovered and selected rows.
- UX review (docs/UX_REVIEW.md), first pass:
  - In the dark theme, status badges and alerts get their colour back: Ready, Waiting, Pending and Open had all looked
    the same grey, and so had warnings such as "not encrypted".
  - Badges are in sentence case, and dialog and drawer titles are bold.
  - The type scale is one step larger: help text was 11 px.
  - Row actions are 28 px targets, and monospace inputs keep normal labels.
  - Queue rows show one state ("Waiting" or "Blocked · n" instead of READY plus WAITING), and running tasks link to the
    run working on them. "Next" is one line, with the reasons a task waits in a tooltip.
  - Messages shows people and agents by default, with SpaceMan's own notifications under a System filter.
  - Attention shows "Never backed up" instead of OK when no backup has run.

## [0.1.0-rc.6] - 2026-11-05

Sixth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): private repositories cloned
with SSH keys SpaceMan holds. Central and the node agent both need this release (node protocol 1.7).

### Added
- Repository SSH keys held by SpaceMan (ADR-0057, security-relevant). A repository with an SSH URL can have an Ed25519
  key that central generates and keeps sealed; only the public key is shown, to add as a read-only deploy key. The node
  clones and fetches with it over SSH, against pinned host keys (GitHub built in, `known_hosts` per repository for
  others), and agents never receive it.
  - API: `POST`/`DELETE /spaces/{id}/repositories/{id}/ssh-key`.
  - CLI: `spaceman repo ssh-key` and `spaceman repo ssh-key-remove`.
  - UI: the repository's SSH key card.
  - The node protocol goes to 1.7.

### Fixed
- Repository clone and fetch errors caused by SSH host keys are now one line that says what to fix, instead of ssh's
  warning banner.

## [0.1.0-rc.5] - 2026-11-04

Fifth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): control starts for waiting
work and stops when idle, nodes can be edited (and provisioned with Terraform), and the step-up prompt asks for the
authenticator code.

### Added
- Control starts and stops on its own (ADR-0056). When work waits for control (an inbox item, a task to decide on, a
  worker that ended, a message) and no control run is live, SpaceMan launches the space's control role, resuming the
  last control's session. It skips this while the space is paused or its node is away. When the launch is refused it
  raises a human action, and it stops after six starts an hour. A control idle for 30 minutes is stopped, and the next
  work starts it again. These are two new space limits, `control_autostart` (on by default) and
  `control_idle_stop_minutes` (30; 0: never), on the Limits page as **Start control on new work** and **Stop idle
  control**.
- `PUT /api/v1/nodes/{id}` and `spaceman node update` edit a node's name, description and labels (with `If-Match`). The
  Terraform provider's new `spaceman_node` resource uses them to register nodes and hand out enrollment tokens.

### Fixed
- The "Confirm it's you" step-up prompt opens above the dialog that triggered it, not underneath it. For an account with
  an authenticator it asks for the code together with the password. Before, it asked for the password only and then
  showed "multi-factor authentication required".
- A restarted control resumes its session only when the session finished a turn. Resuming a session that was never
  saved fails, so such a restart now starts a fresh session on the same instance.

## [0.1.0-rc.4] - 2026-11-04

Fourth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md), with a fix found on the
first deployment.

### Fixed
- A new run is told about messages an earlier run of the same addressee saw but never acknowledged. Before, a control
  launched after one that ended without handling its notice (for example because its account was refused) was never
  told about the waiting inbox item.

## [0.1.0-rc.3] - 2026-11-04

Third release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): the first with images on
Docker Hub.

### Added
- Release images on Docker Hub (ADR-0055): `docker.io/aurimasniekis/spaceman` and `docker.io/aurimasniekis/spaceman-agent`
  for linux/amd64 and linux/arm64, pushed by the release workflow for every `v*` tag, with SBOMs and provenance
  attestations.

## [0.1.0-rc.2] - 2026-11-04

Second release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md). Replaces 0.1.0-rc.1, whose
unpublished drafts were withdrawn: Codex runs with an OpenAI API key did not start there.

### Fixed
- Codex runs with an OpenAI API key start working instead of stopping at Codex's sign-in screen: interactive Codex
  0.166.0 ignores `OPENAI_API_KEY`, so the node now also stores the key as an API-key login in the instance's
  `CODEX_HOME` (ADR-0054). Found by a dry run of the new real Codex suite.

### Added
- `make test-real-codex` (a real Codex turn through SpaceMan) and `make test-real-s3-external` (the artifact store and
  backups against your own S3 or R2 bucket), with a checklist for verifying a release on another server
  (docs/TEST_STRATEGY.md).

## [0.1.0-rc.1] - 2026-11-04

First release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md).

### Fixed
- Terminal notices reach agents after their first turn. herdr-core's idle gate refuses a program that reports `done`,
  which Claude Code does after every turn, so new inbox items and messages were never announced; a refused notice now
  also retries at the next idle report instead of 5 minutes later (ADR-0052).
- Leaving a run page and coming back no longer hangs the browser tab when the run is a full-screen program such as
  Claude Code (a ghostty-web 0.4.0 problem with disposed terminals, avoided by resetting the terminal first).
- Control is told about tasks someone else adds to the queue (like new inbox items) and about tasks someone else makes
  ready, for example from the backlog, so an operator's task no longer waits until control happens to look at the
  queue (SM-CTRL-002).
- The run terminal follows the cursor and scrolls with the mouse wheel, so the bottom of a tall terminal is reachable.
- Several SpaceMan tabs no longer exhaust the browser's connections over plain HTTP: tabs share one change-feed stream,
  and hidden tabs release their live terminal.

### Changed
- **Security:** a diagnostic run's launch needs `run:diagnose` on top of `run:control` (SM-CTRL-004, ADR-0050).
  Operators now hold `run:diagnose`, so operators and admins keep launching diagnostics; viewers never could.
- **Security:** an incident task from a source the queue policy does not trust no longer jumps the queue or preempts
  work; by default it becomes a proposal (ADR-0050).
- Purging a space deletes its runs' recordings and transcripts from the artifact store after the purge commits; they
  were left in the data directory before (ADR-0048).
- The Terraform provider moved to its own repository, `terraform-provider-spaceman` (module
  `github.com/aurimasniekis/terraform-provider-spaceman`), with its history (ADR-0047). This repository no longer contains
  it, and `make check`, CI and the Makefile no longer refer to it: its unit and acceptance tests run there, and the
  acceptance tests build central from a SpaceMan checkout (`SPACEMAN_SRC`, or `../spaceman`).

### Added
- Codex ChatGPT logins (SM-ACCT-002, ADR-0053, migration 0036): the account kind `codex_chatgpt_login` takes the
  `auth.json` that `codex login` writes. The node keeps one live copy per account, which every Codex instance there
  links to, so Codex's own refreshes survive later launches; a rotation replaces it. Use one login on one node.
- Codex runs get their skills in `$CODEX_HOME/skills` (SM-MCP-004; loading verified with codex-cli 0.166.0).
- Starter roles (SM-PROMPT-005, ADR-0051): a new installation gets example harness profiles, prompts and role templates
  for control, planner, implementor, reviewer, kb and dreamer, added once and editable or deletable. A space gets
  ready-to-launch roles from them with **Add starter roles**, `spaceman role add-starters`, `POST
  /spaces/{id}/starter-roles`, or `starter_roles` when it is created (ticked by default in the UI).
- `scripts/dev-stack.sh restart DIR`: rebuild and restart central on a dev stack's existing data.
- Agent setting `runner.terminal_size` (default `160x48`): the size programs start at, instead of herdr-core's 200×120.
- Role templates and harness profiles can be deleted (`DELETE /role-templates/{id}`, `DELETE /harness-profiles/{id}`,
  `spaceman role template delete`, `spaceman profile delete`, the Roles page) while nothing uses them.
- Incident policy (ADR-0050, SM-QUEUE-009, migration 0034): a space's queue policy names the **trusted triggers** that
  may raise incidents (`operator`, `control`, `subscription:<id>`, `template:<id or slug>`; default operator and
  control); an incident from any other source — including one control relays from a worker's, schedule's or another
  space's inbox item — is **proposed** (an ordinary task an operator confirms or dismisses, `POST
  /spaces/{id}/tasks/{task}/incident`, `spaceman task incident`) or **refused**, as the policy says. Preempted tasks'
  workers **finish** (told to wrap up), are **paused** (their turn interrupted with the existing `run.interrupt`
  command) or are **interrupted** (stopped, session resumable), recorded per run in a preemption log and audited;
  **incident capacity** adds top-level slots for incidents; **resumption** is manual (control is told) or automatic
  when the incident ends, and `POST .../resume`, `spaceman task resume` and the control tool `spaceman_task_resume`
  resume one task. Control gets a notification for each step. Queue policy dialog, task drawer (confirm, dismiss,
  resume, preemption log) and Terraform `spaceman_space_queue_policy` fields.
- Human-action type permissions (ADR-0050, SM-HUI-008): per space and type, who sees, asks with and responds to it
  (weakest role) and which agent roles may ask; enforced in the lists, counts, reads, responses and creation; managed
  by space admins (`human_type_permission:write`) under `/spaces/{id}/human-type-permissions`, in the space's human
  actions page, `spaceman human permission` and Terraform `spaceman_human_action_type_permission`.
- User profile edits (ADR-0050, SM-API-004): `PUT /users/{id}/profile` (global admin, step-up, If-Match, audited)
  changes a user's email, display name and administrator MFA setting; the users API returns `mfa_required_set` beside
  the effective `mfa_required`. **Edit** on the Users page, `spaceman user update`, and Terraform `spaceman_user` now
  updates these fields in place instead of refusing them.
- S3 artifact store (ADR-0048, SM-STORE-002): `artifacts.backend: s3` keeps attachment content, central's copies of
  recordings and run transcripts in an S3-compatible bucket (`artifacts.s3`, keys from files, TLS always verified) under
  the same keys as the local layout. Uploads are spooled and sent as single PUTs whose SHA-256 the service checks;
  downloads stream, answer range requests with range GETs, and fail at the end when attachment or transcript content is
  damaged. Recordings are spooled while a node ships them and archived when they close or their run ends; transcripts
  are uploaded once received and checked; an upload the bucket refuses stays on the local disk, is served from there and
  is retried at the next start. Retention, purge and garbage collection delete the objects; quotas are unchanged.
  Backups copy every S3 artifact into the sealed backup and record the store in the manifest (`artifact_store`). The new
  offline command `spaceman artifacts migrate --to s3|local [--remove-source] [--dry-run]` moves stored artifacts, and
  `spaceman backup restore` names it when the installation uses S3. Configuration validation refuses an
  `artifacts.s3.prefix` that overlaps `backups.s3.prefix` in the same bucket. Operators' page: Artifact storage.
- Go interfaces for the remaining adapter kinds (SM-ARCH-003, ADR-0048): `artifacts.Store` (local and S3),
  `events.Source` (Pulse durable consumers and core NATS, picked by delivery mode) and `gateway.Transport` (streamable
  HTTP). The implementations are unchanged; tests drive each through a fake.
- Paused-space event policies (ADR-0049, SM-EVT-014): each subscription's `while_paused` is `defer` (default: keep
  ingesting, tell control about new items when the space resumes), `wake` (tell control at once) or `stop` (stop until
  the resume; durable events wait in the stream). Deferred notices go out on resume and at central's start, without
  polling. Subscribe dialog and subscription page, `while_paused` in the API, `--while-paused` in the CLI; inbox items
  show **control told on resume**; subscriptions report the state `paused` (also in `spaceman_ingest_subscriptions`).
- Bounded automation across installations (ADR-0049, SM-EVT-016): outbound events carry the SpaceMan header
  `spaceman-depth` (how many SpaceMan ingestions led to the change); inbox items and tasks created from them record
  their `automation_depth`; an inbound event at or over the space's new limit `event_depth` (default 8, 1–64), or with
  an unreadable depth, is quarantined with the reason. Reprocessing it ingests it one level deeper.
- Routed human responses as inbox items (ADR-0049, SM-INBOX-001): the human-action policy field `response_inbox`
  (`off` by default, `routed`, `always`) makes answers also inbox items with the source `human_response`. The item
  names the action and its routing, never the response, which stays on the action. Policy dialog, API,
  `--response-inbox` in the CLI.
- Migration 0033 (`while_paused`, `notice_deferred`, depth columns, `response_inbox`).
- Space repositories (ADR-0044, SM-REPO-001/003, SM-SPACE-001, SM-UI-002, SM-API-004): a central list per space (name,
  folder, clone URL without credentials, default branch, role code or kb, clone policy auto, on_demand or manual) with
  API CRUD under `/spaces/{id}/repositories` (If-Match, audited), a sync endpoint, the node protocol command
  `space.repos.sync` (protocol minor 4) that clones, fetches and reports with the node's own Git authentication, a space
  **Repositories** page with the node's report (remote, default branch, last fetch, changes, unpushed commits, worktrees
  and their runs), worktree links on runs and tasks, the read-only agent tool `spaceman_repos_list`, `spaceman repo`
  commands and the Terraform resource `spaceman_space_repository`. Workers' worktrees and first launches clone configured
  repositories; relocation clones configured repositories on the destination. Migration 0029 lists every knowledge
  base's folder as a repository (role kb, manual); knowledge bases are unchanged.
- Retention (ADR-0045, SM-REC-003, SM-HUI-007, SM-ATT-006, SM-SPACE-001): retention periods for recordings, transcripts,
  attachments of finished items and finished human actions are limits (`limits.retention_*_days`, overridable per space
  on the Limits page, API, CLI and Terraform `spaceman_space_limits`; 0 keeps, the default), and audit retention is
  installation-wide (`retention.audit_days`). One daily pass on the shared deadline timer deletes what is older, never
  what an open item uses and never before a restore is acknowledged; every deletion is audited and in the change feed.
  Status and a manual pass: Backups page **Retention** card, `GET /retention`, `POST /retention/run`,
  `spaceman system retention status|run`. Changing a retention period needs the new `retention:write` (admins).
- Space export (ADR-0045): `GET /spaces/{id}/export` streams a ZIP of the space's inbox items, tasks, human actions with
  responses and comments, messages and attachments (JSON Lines plus files, with a checksummed manifest), from one
  consistent snapshot; `GET /spaces/{id}/export/summary`, `spaceman space export summary|download`, an **Export** dialog
  on the space overview. Admins only (`space:export`), audited, bounded by the space's `export_max_bytes` (default
  2 GiB). The archive is **not encrypted**, and says so everywhere it is offered.
- Configurable recording size: the space limit `recording_quota_bytes` (default 512 MiB, 1 MiB–64 GiB) is sent to the
  node with each launch (SM-REC-003).
- Durable forward audit cursor (SM-OBS-003): `GET /audit?after=<seq>` lists events oldest first with `next_after` to
  resume from, and a `gap` when retention pruned events past the position; `spaceman audit list --after`. The audit page
  shows when older events were pruned.
- Backups flush role memory first: central asks connected nodes to checkpoint and sync (node protocol minor 5,
  `memory.flush`), waits at most a minute, and the manifest, the Backups page and the `backup.finished` audit event name
  spaces whose memory is possibly stale (disconnected or older nodes, timeouts, unacknowledged history) (SM-MEM-005).
- SSO-only sign-in (password sign-in off for the installation), with a lockout precondition and guards, as in Hallpass:
  Sign-in providers page, `PATCH /api/v1/oidc/settings`, `spaceman oidc settings set --sso-only` (SM-AUTH-001).
- Direct sign-in through one provider (`/login?password=1` shows every method).
- RP-initiated logout per provider (`end_session_enabled`, API, UI, CLI `--end-session`, Terraform
  `spaceman_oidc_provider.end_session_enabled`): signing out of an SSO session offers sign-out at the provider.
- Attention summary (SM-UI-003): an **Attention** page, `GET /api/v1/attention` and `spaceman system attention` gather
  what needs a person: pending, overdue and escalated human actions, blocked and failed tasks, quarantined events,
  unknown and recently failed runs, budget notices, attachment storage near its quota, spaces whose node is not
  connected, and, with `system:read`, disconnected nodes, outbound dead letters, failing backups and central's storage
  health (readiness checks, database state, data-disk free space against its reserve). Each category is filtered by the
  caller's permissions; one grouped query per category, refreshed from the change feed. Each space overview shows its
  counters.
- Terminal takeover (SM-UI-004, ADR-0046, **security-relevant**): an admin holding `terminal:takeover` can take over a
  running terminal from the run page (and `spaceman run takeover`). Keystrokes go through herdr-core's interactive
  attach (contract S-5), are bounded and never stored or logged; every automated input (Message the agent, message and
  human-response notices, queued node input) waits until the takeover ends; every viewer sees who holds it; it is
  audited and ends on release, expiry (30 s without renewal), run end or node failure. Node protocol 1.6.
- Human actions (SM-HUM-006, SM-HUI-001, SM-HUI-006): server-side search (`q`) over title and text, filters for status,
  assignee (`me`, `unassigned`), priority, requester, due date and space, and display sorts, on both lists and the
  CLI; **Ask the requester** clarification requests (`POST …/clarify`, a durable `clarification` notice routed like a
  response) and the agent tool `spaceman_human_reply`; links from task blockers and actions to the requester run, task,
  inbox item and follow-up; per-user notification preferences (`/auth/me/notification-preferences`, Account →
  Notifications, `spaceman notifications`) for in-app and desktop notifications.
- Attachments beyond the inbox (SM-ATT-001/002): tasks, human-action requests, responses and comments, and messages and
  replies reference attachments (`attachment_ids`), written in the TipTap composer with paste, drop and attach.
- Space archive, restore and purge in the web UI (SM-SPACE-002), with confirmation dialogs; purge asks for the slug and
  a recent sign-in with MFA. Spaces lists archived spaces on request.
- Task reassignment in the task drawer and `spaceman task update --run` (SM-CTRL-004); the audit record of a task
  update names the previous and new assignee, and a run of another space is refused.
- Custom-definition subscriptions in the subscribe dialog (SM-EVT-017): a JSON editor with validation as you type and a
  sample-event preview through the preview API; a custom subscription shows its definition.
- TanStack Table (sortable, filterable shared table on the attention and nodes pages) and TanStack Store (per-browser
  view preferences) are now used (SM-UI-001); every form uses TanStack Form, including the schema-driven human-action
  fields (SM-ATT-007).
- Migration 0031: clarification notices and comment kinds, notification preferences, terminal takeovers.
- Release readiness report (`docs/RELEASE_READINESS.md`, site: Development → Release readiness): what is implemented,
  tested and verified for real, what is not, and what a production release still needs. It makes no production-readiness
  claim.
- Documentation site completed (SM-DOCS-001…005): new **Architecture**, **Deployment overview** (topology, install paths
  and a recovery map), **Troubleshooting** (startup errors, readiness codes, node connection, leases, recordings,
  sign-in) and **Publishing the site** pages; a **Configuration reference** generated from `internal/config`'s types,
  doc comments and defaults (`make config-docs`, drift test `TestConfigReferenceUpToDate` in `make check`, also checked
  by `make site-check`); a glossary of coordination terms on Concepts; space archive, restore and purge in the first-space
  guide; a `kb-sessions` screenshot of a knowledge base's support sessions. `check-mermaid.mjs` now also fails on
  unquoted flowchart labels, and `check-shots.mjs` on an inventoried screenshot that no page shows. The site is **not
  published**: the Pages workflow has never run.
- Complete remote CLI coverage (SM-API-003, ADR-0040 amendment): commands for 239 of the 248 API operations (was 132).
  New groups `kb`, `memory`, `message`, `dream`, `approval`, `attachment` (upload, download), `oidc` (providers, group
  mappings), `identity`, `session`, `mfa`, `password` and `watch` (the change feed, JSON Lines with `-o json`); new
  commands for task blockers, dependencies, escalation, decisions and the queue policy, run input, usage, recordings and
  transcripts (download to a file, delete with `--yes`), human-action creation, assignment, comments, types and policies,
  a space's accounts and MCP servers, prompt diff, fork, rollback and draft discard, skill creation from a directory,
  event template preview, subscription quarantine (reprocess, discard), user reset tokens, link invitations and
  identities, and the outbox, restore, break-glass and root-token notices of `system`. One-time secrets of the new
  commands are shown only on request (`--secret-file` or `--show-secret`); downloads go only to `--out` (new `0600`
  file, `--force` to replace, never a terminal). The nine operations left out are browser sign-in flows, listed with
  their reasons in the CLI guide and reference; `TestEveryOperationHasACommand` fails when a new operation has neither a
  command nor a recorded exclusion.
- Core NATS event subscriptions (SM-EVT-011, ADR-0042): `delivery: core` on a subscription (API, subscribe dialog, CLI
  `--delivery`, Terraform `delivery`) subscribes with plain NATS on the Pulse connection, for sources that publish
  without JetStream. Same filters, schema, quarantine and inbox items; deduplicated by `Nats-Msg-Id` or, without one, a
  content hash. Clearly labelled **non-durable** (API schema, a badge and explanation in the UI, the events guide): no
  replay, no redelivery, no cursor, and messages are lost while disconnected. Replay and start-now are refused; a missing
  `subscribe:<subject>` grant is reported on the subscription. Migration 0028.
- Ingestion metrics on the management listener's `/metrics` (SM-EVT-013, SM-OBS-002): `spaceman_ingest_deliveries_total`
  (delivery mode, origin, result: created, duplicate, filtered, quarantined, error, rate_limited),
  `spaceman_ingest_processing_duration_seconds`, `spaceman_ingest_consumer_pending` and `_ack_pending` (JetStream
  consumer lag), `spaceman_ingest_core_dropped_total`, `spaceman_ingest_subscriptions` and `spaceman_ingest_connections`
  by state, `spaceman_ingest_quarantine_open` and `spaceman_ingest_replays_total`. Labels are fixed enums only. New
  operators' page **Observability** with the full metric catalog and suggested alerts.
- Scripted end-to-end demonstration (SM-VER-003): `make demo` (`scripts/demo.sh`, driver `test/demo` behind the
  `demo` build tag, browser steps `web/e2e/demo.spec.ts`) runs the thirty steps of the demonstration in order against a
  real central with the embedded UI, two real agents with their own herdr-core cores, devpulse and devmcp, through the
  browser, the API and the CLI, and writes one evidence line per step (outside the repository) plus a summary table. Stub
  harness by default; `SPACEMAN_DEMO_REAL=1` with `SPACEMAN_DEMO_CLAUDE_TOKEN` makes the Claude Code worker a real,
  budget-bounded run. Guide: `docs/DEMO.md`, site page Development → End-to-end demonstration.
- Stub harness (development only): it now finds the SpaceMan MCP server when it stands in for Codex (`-c mcp_servers.…`
  overrides), and understands `/await-human [n]`, `/kb-delay <seconds>` and `/dream`; KB answers cite the working
  tree's revision.
- Real-dependency suites (SM-VER-002), opt-in behind the `real` build tag and a variable each, each removing only what
  it created: `make test-real-porch` (SpaceMan signs in through a real go-porch built from the sibling checkout:
  `porch.groups` mapping, group JIT, replay refusal, step-up), `make test-real-pulse` (real pulsed, nats-server and the
  go-pulse-github service with its own fixtures: fan-out to two spaces, dedup, replay, quarantine, loop guard, outbound
  publication), `make test-real-s3` (a throwaway MinIO container pinned by digest: backups, list, verify, tamper
  detection, offline restore), `make test-real-systemd` (`scripts/systemd-smoke.sh`: the linux release archive under
  systemd in a Debian 13 container, both units), and `make e2e-browsers` (the browser journey in Firefox and WebKit).
  Results from 2026-11-03 are in `docs/TRACEABILITY.md` and `docs/TEST_STRATEGY.md`. Codex, AWS S3 and a physical Linux
  host remain unverified (`docs/LIMITATIONS.md`).
- Remote command line (SM-API-003, ADR-0040): `spaceman` is now also a client for a running central's API
  (`--server`/`SPACEMAN_SERVER`, an API token from `--token-file`, `SPACEMAN_TOKEN_FILE` or `SPACEMAN_TOKEN`,
  `-o table|json|yaml`). Commands for nodes, spaces (including `space relocate precheck|start|status|retry|cancel|release`
  and the unavailable-source flow with an explicit acknowledgement), runs, inbox, tasks, human actions, schedules,
  limits, accounts, prompts, skills, harness profiles, roles and role templates, event connections, templates and
  subscriptions (with replays), MCP servers and grants, approval policies, cross-space peers, requests and settings,
  users, API tokens, audit and backups (`system backup`). Updates send `If-Match` with the version read (or
  `--if-match`), creates send an `Idempotency-Key`, `--all` follows cursors, problems are printed with their code,
  fields and request ID, and exit codes are documented (0 ok, 1 usage, 2 API error, 3 conflict, 4 not found, 5 auth).
  Generated reference in `site/reference/cli/` (`make cli-docs`, drift-checked), guide in `site/operators/cli.md`.
- API: read operations for objects that only had a list: `GET /event-connections/{id}`, `/harness-profiles/{id}`,
  `/role-templates/{id}` (ID or slug), `/spaces/{id}/roles/{name}`, `/approval-policies/{id}` and
  `/spaces/{id}/xspace/peers/{peer_id}`. The API reference now also describes the attachment-content and transcript
  downloads.
- Space relocation (SM-INV-003, SM-SPACE-005…008, ADR-0037): an operator moves a stopped space to another node from the
  space page (**Relocate**), the API (`/spaces/{id}/relocation`, with precheck, retry, cancel and release) or Terraform.
  The source syncs role memory and uploads, and its repositories are checked; unpushed, changed, untracked and stashed
  work is refused unless explicitly exported through central. The destination is prepared and verified, then the
  assignment and the execution lease move in one step with a new generation that fences the source; the space stays
  paused and the source folder is retained (or deleted). An unavailable source can be replaced from central's copies with
  an audited acknowledgement of possible loss. Runs on the destination start fresh native sessions (project continuity
  only). Node protocol minor 3; migration 0026.
- Terraform: a change of `spaceman_space.node_id` now relocates the space in place (`relocation_dirty_state`,
  `relocation_stop_runs`, `relocation_recovery`, `relocation_retain_days`, `relocation_timeout`) instead of being refused.
- Terraform provider phase 2 (SM-API-004, SM-HUI-008, ADR-0041): `spaceman_space_limits` (owns only the configured
  overrides; removing one clears it, destroy clears the owned ones), `spaceman_space_queue_policy` (top-level task
  limit), `spaceman_schedule`, `spaceman_space_xspace_settings`, `spaceman_xspace_peer` (one side of a cross-space grant,
  owned whole), `spaceman_human_action_policy` (installation defaults and space overrides), `spaceman_user` (disabled on
  destroy), `spaceman_user_grants` (the whole manual grant set) and `spaceman_user_identity`; data sources
  `spaceman_space_limits` (effective values and sources) and `spaceman_user`. Creating a peer configuration or a
  human-action policy that already exists fails instead of overwriting it. Passwords, reset tokens, link invitations and
  MFA enrollment are never managed.

### Security
- Retention, space export and SSO-only sign-in are security-relevant changes (ADR-0045): shortening a retention period
  (admins, `retention:write`) deletes data at the next pass; exports are plaintext and admin-only; SSO-only is guarded
  against an administrator lockout.
- Security review (M4-08, `docs/SECURITY_REVIEW.md`, ADR-0039). The fixes:
  - **Login and MFA throttles hold under parallel attempts.** Attempts on one account or second factor are serialized,
    so a burst can no longer try many TOTP codes in one throttle window.
  - **The node daemon no longer runs code from repositories agents can write.** Writer worktrees are prepared with
    hooks and fsmonitor disabled. Git refuses to run in a repository whose own configuration defines a program, such as
    a filter, merge or diff driver, SSH or proxy command, credential helper, include or URL rewrite. This covers memory
    checkouts, KB and writer worktrees, and relocation.
  - **Relocation keeps everything inside the destination's space folder.** Worktree paths are validated centrally and
    on the destination and may not pass through a symlink. Chained symlinks in the loose-file archive and a symlink in
    place of `instances/` are refused.
  - **Purging a space now forgets its grants immediately.** The authorization engine used to keep them in memory until
    its next reload.
  - **Concurrent requests with one `Idempotency-Key` create the resource once.**
  - **Agent tools:** only a recipient may reply to a message, and so resolve it.
  - **API:** deleting a transcript needs re-authentication with MFA, like deleting a recording. Recording a queue
    decision (`GET …/queue/next?record=true`) needs `queue:write`.
  - **Event templates:** instructions may only use the template's variables (`vars.*` placeholders) and never contain event text.
  - **Documentation:** the threat model and the Security page now state every SM-SEC-001 boundary, and
    `make traceability` checks that they keep doing so.
- Cross-space requests (SM-XSPACE-001…003, ADR-0038): a space's control can ask another space a question, delegate
  work to it or ask one of its knowledge bases, only as far as both spaces allow. Each space sets, per peer, what it
  accepts (discovery, questions, delegation, knowledge per KB, what its replies disclose, quotas) and what its own control
  may send; everything is off by default. Questions and delegation land in the destination's inbox for its control,
  which accepts (optionally creating its own task), declines or replies; knowledge requests go to a support session of
  the destination's KB. Replies are reduced to the destination's disclosure level before they are stored. Requests carry
  correlation, deadlines, cancellation, depth and the spaces their chain visited (loops and over-deep chains are
  refused) and are bounded per pair and per source. Only control runs have the eight `spaceman_xspace_*` tools. New
  **Cross-space** section per space (requests and access per peer) and a global **Cross-space** page; `xspace:read` /
  `xspace:write` permissions; events `xspace.request.created` and `xspace.request.state_changed` (metadata only).
- Revocation reaches open streams at once (SM-AUTH-007, ADR-0033): signing out, revoking a session or API token,
  rotating the root token, disabling a user, resetting a password or changing grants ends affected live terminals and
  change-feed streams as soon as the change commits. A removed space grant hides that space's changes immediately and
  ends its terminals with "Access lost". Expiry, idle timeout and changes made while central was stopped are caught by
  a periodic re-check (`auth.stream_revalidate`, default 30 s), which now also re-checks node connections' credentials.
  The web UI returns to sign-in when its session is gone and reconnects when it was only rotated.

### Changed
- `POST /api/v1/auth/logout` returns 200 with `{provider_logout_url}` instead of 204.
- Deleting central's copy of a recording (`DELETE /spaces/{id}/runs/{run}/recording`) needs the run to have ended
  (409 otherwise) and is now audited (`recording.deleted`) and recorded in the change feed.
- Backup verification compares the audit chain's head sequence and hash with the manifest, not the event count, so
  backups taken after audit retention verify.
- `auth.stream_revalidate` accepts at most 60 s (was 5 min).
- Launches can now be refused with `rate_limited`: a space runs at most `limits.max_concurrent_runs` worker and diagnostic
  runs (default 8), and the node's reported `capacity` is enforced. Control, KB support and dreamer runs have their own pools.
- KB support sessions can no longer ask knowledge bases (support depth 1 by default), so support never starts more support.
- Event deliveries record their origin (live, redelivery, replay, reprocess) and attempt; replays are recorded in a replay
  history and items they create are marked.

### Fixed
- Relocation's flush could report success after its context was cancelled.
- A space's lease guard no longer outlives its runs (SM-ARCH-004, ADR-0043). The node agent kept one
  `spaceman-agent lease-guard` process per space that had ever run, reading the lease file once a second while the space
  sat idle. The guard now ends with the space's last live run, and the next launch starts a new one before its run.
  Enforcement for live runs is unchanged.
- The Architecture page no longer says recordings, attachments and transcripts can be kept in S3. They are stored on
  central's local disk; only backups have an S3 target.
- Central no longer refuses to start (or to run `spaceman backup`) when the command-line client's (`SPACEMAN_SERVER`,
  `SPACEMAN_TOKEN_FILE`, `SPACEMAN_OUTPUT`, `SPACEMAN_SPACE`, `SPACEMAN_ALLOW_INSECURE_HTTP`), enrollment's
  (`SPACEMAN_ENROLL_TOKEN`) or the Terraform provider's (`SPACEMAN_ENDPOINT`) variables are set; they were rejected as
  unknown configuration overrides. `SPACEMAN_ALLOW_NETWORK_FS=1` (development mode only) now takes effect for the same reason.
- Documentation brought in line with the code: harness accounts exist (try-locally), inbox item sources, the
  concurrent-runs and storage-limit names, what the dev stack starts, backups without `git` leave role memory out
  instead of failing, what `/readyz` checks, backup and restore commands run as the service user with the systemd
  configuration path, root-token recovery with `spaceman admin root-token rotate`, lease-guard evidence (macOS only),
  and the Launch run and Add dream form fields. Accepted-but-unused settings (`auth.local_login_disabled`,
  `nodes.command_timeout`, the agent's `management_listen` and `harness.git_binary`) and the unimplemented S3 artifact
  store are stated in the configuration reference and the known limitations.
- Stale texts: the `spaceman_human_ask` tool no longer calls approvals "advisory for now", the root-token output names
  the Account page, and the corrupt-database error points at the backup guide.
- Documentation: the Porch setup now says to add `groups` to the application's allowed scopes. Without it Porch drops
  the scope and SpaceMan sees no groups (found by the real Porch suite).
- Recordings on a full disk: the run continues as before. Central's copy now ends with a `trunc` record (`disk`) and
  the run's close record instead of stopping silently. A quota truncation also reaches central's copy, and a recording
  resumed after a local loss continues above central's sequence numbers.
- A stopped node agent ends its recorders. In-process restarts used to leave two recorders on one run.
- A storage-busy API error (503 `unavailable`) now carries `Retry-After`.
- `make test-pulse` ran a directory that did not exist. It now runs the Pulse/NATS integration tests. New
  `make test-fault` suite (`test/fault`).
- Two tests that failed under heavy machine load (`TestRunKickAndRetryTimer`, `TestLoginThrottlingAndEnumeration`) now
  wait for the right condition and check that the dummy hash runs, without weakening what they assert.
- Live updates: a browser whose change-feed position is ahead of the server (after a restore from backup) now gets a gap
  and reloads instead of silently skipping changes.
- A space's goals and standing instructions now reach every run's system prompt (the launch dialog already said so).
- Accessibility (WCAG 2.1 AA): contrast of dimmed text, accent colors and badges in both themes; named close buttons,
  account menu, replay slider and task-list checkboxes; space sections are navigation links; the docs site's search
  button, links and code colors.

### Added
- Limits and budgets (SM-LIMIT-001/002, ADR-0036): one limits model with installation defaults (`limits:`), per-space
  overrides and a single effective read with sources (API and the space's **Limits** page). Concurrency, tool calls per
  run, event ingestion per subscription, KB support sessions, pending human actions, storage, task deadlines and
  usage-based token and cost budgets. Lowering a limit never stops running work unless the space chooses `on_lower: stop`.
  Budgets warn and stop new runs; they are not a hard currency limit, because usage is reported late.
- Schedules (SM-LIMIT-003): inbox items or tasks at cron or interval occurrences in an IANA time zone, with skip, run-once
  or bounded catch-up for missed runs and per-occurrence deduplication, driven by the single deadline timer (the space's
  **Schedules** page and API).
- Explicit replay of a subscription with a reason (`POST …/subscriptions/{id}/replay`), its history, and an option to
  create fresh items (SM-EVT-015).
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
- Attachments: files uploaded to a space with sniffed types, size and quota limits, safe serving and garbage collection.
- Inbox per space: operator items in Markdown or JSON with attachments, urgency, statuses (in progress, deferred, rejected,
  done), deduplication by source key, and a TipTap editor that accepts pasted, dropped or attached images and files.
- Pulse events: connections with encrypted credentials, a catalog of versioned event templates (built-in GitHub issue and
  pull request templates), space subscriptions with one durable consumer each, explicit cursors (start now, replay),
  delivery history, quarantine with reprocessing, and template testing with sample events.
- Execution queue per space: one explicit order (priority on arrival, operators after), incident policy, dependencies with
  cycle detection, explicit blockers, exclusive resources, an active-task limit, selection with recorded skip reasons, and
  drag or keyboard reordering. Inbox items can become tasks.
- Human actions: questions, approvals (advisory for now), to-dos and messages for people, separate from the inbox and the
  queue, with type-specific responses, claiming, comments, withdrawal, expiry and follow-ups. An action can block its
  task until someone responds. A "Needs you" page lists pending actions across the spaces you can read.
- Accounts: Claude subscription tokens, Anthropic and OpenAI API keys stored sealed and never shown again, allowed per
  space with a default per harness. Launches pick the account deterministically and refuse clearly when none fits;
  rotation applies to later launches.
- Agent tools: every run's SpaceMan MCP server now serves the tools its kind may use, over the node connection: reading
  and adding inbox items, reading and managing tasks, fetching attachments, and asking people through human actions.
- Messages: durable messages and requests between agents, control and operators, with delivery states SpaceMan tracks.
  Idle agents get a one-line notice in their terminal; content is read through their tools. Human responses reach the
  agent that asked the same way. Each space has a Messages tab.
- Control and workers: control starts and stops workers for eligible tasks through its tools, each writer in its own git
  worktree (never reset or deleted). Control is notified of new inbox items, tasks awaiting review and ended workers,
  and is restarted with its session resumed if it stops unexpectedly, at most three times an hour.
- Roles and prompts: versioned prompts (drafts, publish, extend, fork, diff, rollback), harness profiles, role templates
  and space roles. Launches resolve them in a documented order with a permission-mode ceiling, and each run records its
  configuration in a manifest.
- Usage and transcripts: runs report token usage read from the harness's own records (Claude Code transcripts), with
  the harness's cost estimate and its assumptions; unknown usage is shown as unknown. Ended runs upload their native
  transcript, which can be downloaded or deleted.
- Dreams: a dreamer agent consolidates role memory from new transcript segments on demand or on a schedule, within
  time and evidence budgets; it reports findings, and the evidence it read counts as done only after its memory
  changes are checkpointed. A built-in prompt describes the method.
- Knowledge bases: agents ask a space's KB repositories through their tools; each requester gets its own support
  session (queued at capacity, put to sleep when idle, recovered when it fails); answers cite evidence and say whether
  they rest on accepted knowledge; proposals are accepted on the node after the KB's check, never forced.
- Role memory: each role's notes are Git history on the node, shared by its instances, checkpointed at idle points and
  run ends, merged without overwriting (conflicts are preserved and reported) and preserved centrally by fast-forward
  only. Spaces have a Memory section.
- Enforced approvals: policies make built-in agent tools or gateway tools wait for a person's approval, bound to the
  exact arguments, the requesting run, an expiry and a number of uses; the decision and each execution are recorded
  separately, and the approver's authorization is checked again before execution.
- MCP gateway: administrators register external MCP servers with sealed headers and grant them to space roles. Runs
  see their tools as `<server>__<tool>`, each run with its own upstream session; calls are rechecked, bounded, audited
  and dialed only to addresses the egress policy allows.
- Skills: a central catalog of versioned skills (SKILL.md plus reference files), including a built-in HallPass skill,
  assigned through role templates and space roles and written into each Claude Code run's config folder.
- Custom human-action types: administrators define request and response schemas, outcomes and form hints; spaces enable
  them; people answer through generated forms (or JSON) and agents discover them through their tools.
- Single sign-on: OpenID Connect providers such as Porch, with PKCE (S256), validated ID tokens, sealed write-only
  client secrets and refused private-network providers unless allowed. Group mappings (including Porch's
  `porch.groups`) grant a role in every space or one space, re-applied at every sign-in and only to SSO sessions.
  Pre-provisioned or just-in-time users, identity linking from the Account page, by an administrator or with a link
  invitation, step-up at the provider (or with the SpaceMan TOTP code where allowed), test logins, and a Sign-in
  providers page.
- Publishing SpaceMan's own events to Pulse (off by default, `events.outbound`): run, task, inbox item, human action,
  approval, memory and knowledge-base lifecycle events with versioned, metadata-only payloads, written in the same
  transaction as the change and relayed after commit with stable IDs, retries with backoff, dead-lettering, retention,
  correlation, causation and origin headers. SpaceMan never re-ingests its own events. Publication status (connection,
  backlog, lag, failures) on the Events page for administrators, in the API and as Prometheus metrics.
- Backups and restore: sealed backups (every file encrypted under the KEK, which is never included) of the database,
  role memory (Git bundles), recordings, attachments, transcripts and the effective configuration, with a manifest
  stating what is included and excluded, per-file SHA-256 and a sealed copy of the manifest. Local or S3-compatible
  targets, a schedule with one timer, retention by count and age, verification that opens every file and checks the
  database copy and audit chain, a Backups page, `/api/v1/system/backups` and `spaceman backup create|list|verify|restore`.
  A restore comes up with every space paused and execution authority renewed (lease generations advanced, earlier runs
  fenced, commands cancelled, run credentials revoked), event subscriptions replaying from their oldest stored delivery,
  approvals with uses left revoked and pending ones kept, and a restore notice until the spaces are resumed.
- Human-action follow-up: policies per action type with space overrides (Needs you shows reminded and escalated
  actions; **Policies** on a space's Human actions tab, installation defaults on Action types) for reminders, escalation
  (raise the priority, assign someone; the order never changes), rate limits per agent run and per space, and what
  happens to a response when the requester's run has ended: rerouted at once to a running successor-role run, else to
  control, else shown as undeliverable to people (with a message to operators and **Send to control**), or kept for the
  next run of the same agent instance until a hold ends. Routing is durable across restarts and records the original
  run, the previous target and the reason; responses are never shown to unrelated runs. Expiry, reminders, escalation,
  holds and approval expiry share one timer for the earliest deadline (the once-a-minute expiry job is gone). New
  outbound events `human.action.reminded`, `escalated` and `rerouted`; API under `/api/v1/human-policies`,
  `/api/v1/spaces/{id}/human-policies` and `…/notifications/{id}/route`.
- Packaging (Linux):
  - Reproducible release archives for linux/amd64 and linux/arm64 (`make release-snapshot`). Each holds `spaceman` with
    the UI embedded, `spaceman-agent`, systemd units, example configs and the licence files. They come with SPDX SBOMs
    (when syft is installed) and `SHA256SUMS`.
  - A hardened `spaceman.service`, and a `spaceman-agent.service` whose relaxations for harnesses are explained in
    ADR-0034.
  - A central container image (Debian slim with git), a node agent base image that is labelled and documented as
    **not agent-ready** (no herdr-core, no harnesses), and an example for building an agent runtime image from
    operator-supplied binaries.
  - A central-only Compose file, a container smoke test (`make image-smoke`), and a tag-triggered release workflow
    that only drafts a GitHub release and pushes no images.
  - Installation, containers (with the image matrix) and upgrade pages.
- Terraform provider (now in its own repository, `terraform-provider-spaceman`; phase 1, not published): 20 resources for spaces, space roles and
  account bindings, accounts, harness profiles, role templates, prompts, skills, MCP servers and grants, event
  connections, templates and subscriptions, human-action types and their space enablement, approval policies, knowledge
  bases, dream configurations, OIDC providers and group mappings, plus node, space and event-template data sources.
  Credentials are write-only arguments with a version to rotate them; updates send `If-Match` with the version from
  private state; catalog changes publish exactly one immutable version; UI edits show as drift only for configured fields;
  a `node_id` change is refused until relocation exists. Acceptance tests run against a throwaway local central
  (`make acc` in the provider repository).
- Documentation site (VitePress with Mermaid), real screenshots captured by `make screenshots`, a local development stack
  (`scripts/dev-stack.sh`) and a browser end-to-end journey (`make e2e`).
- Product contract (`docs/GOAL.md`), architectural decisions, execution plan, requirement traceability matrix and verified dependency
  contracts for herdr-core, Pulse, go-pulse-github, Porch, Hallpass, Claude Code and Codex.

[Unreleased]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.0-rc.9...HEAD
[0.1.0-rc.9]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.9
[0.1.0-rc.8]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.8
[0.1.0-rc.7]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.7
[0.1.0-rc.6]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.6
[0.1.0-rc.5]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.5
[0.1.0-rc.4]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.4
[0.1.0-rc.3]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.3
[0.1.0-rc.2]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.2
