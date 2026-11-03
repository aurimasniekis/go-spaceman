# SpaceMan — Known limitations

> Honest list of things SpaceMan does not do or does not guarantee. Each entry links to a requirement or ADR.
> This file is required reading before production use, and it is updated whenever a limitation is found or removed.

## Product boundaries (by design)
- **Trusted-team model.** SpaceMan is not a hostile multi-tenant system. Every operator with a node shell can read and change that node's spaces (SM-SEC-001).
- **No sandbox.** A space folder is not an isolation boundary. Agents with shell access can read other spaces' folders on the same node
  and bypass MCP mediation (SM-INV-007).
- **Runtime secrets are visible on the node.** Harness credentials have to exist in the harness process environment or config dir. Root
  or the agent's OS user on the node can read them (SM-ACCT-005).
- **No approval interception of arbitrary commands.** Enforced approvals cover only operations SpaceMan itself performs (MCP gateway
  tools and built-in privileged operations). Harness permission prompts that SpaceMan does not integrate stay in the terminal or need a takeover (SM-APPR-005).
- **At-least-once events.** Event ingestion is at-least-once, with deduplication on the event ID. Plain NATS Core subscriptions are not durable (SM-EVT-011).
- **No hard currency limits.** Usage data may be delayed or missing. Unknown usage is shown as unknown (SM-REC-005, SM-LIMIT-003).
- **Single active central instance.** No high availability. Recovery is restore-from-backup (ADR-0001).

## Event ingestion
- **Pulse (JetStream) only.** Connections use the go-pulse SDK with `.creds`. Plain NATS Core subscriptions (non-durable) are
  not implemented yet; direct GitHub webhooks are not implemented (SM-EVT-019).
- **Grants are the Pulse operator's job.** Each subscription needs exactly the durable grant its page shows
  (`durable:<subject>:<durable>`). A missing grant shows up as a timeout in the subscription's error, not as a clear
  permission error (go-pulse v0.1.3 behaviour). The in-process test server does not model grants.
- **No live go-pulse-github evidence yet.** The GitHub templates are tested against the event go-pulse-github builds for its
  `issues_opened` fixture (derived from its code), not against a running pulse-github. There is no upstream pull request
  fixture.

## Event publication
- **Off by default; nothing is back-filled.** Events of a period with `events.outbound.enabled: false` are never
  published, and rows left pending when publication is turned off are dropped after the retention (ADR-0030).
- **At least once, no global order.** Consumers must deduplicate on `Nats-Msg-Id`; JetStream deduplicates retries only
  within its 2-minute window. Events of different subjects can arrive in any order (SM-OBS-004).
- **Loop guard is per installation.** An installation drops only events carrying its own `spaceman-origin`. Automation
  depth across installations (an event creating an item, a task and another event) is not bounded yet (SM-EVT-016).
- **No interrupted-execution event.** Approval executions marked interrupted by a central restart are not published.
- **No real Pulse evidence yet.** Publication is tested against the in-process Pulse-shaped NATS server (real JetStream,
  real go-pulse SDK), not against a running `pulsed`. Grants (`publish:evt.spaceman.>`) are not modelled by that server.

## Accounts
- **No Codex ChatGPT login.** Codex runs use an OpenAI API key; ChatGPT-plan logins (`auth.json`) are not supported yet (K-9).
- **No real Codex run yet.** The Codex adapter is tested with fakes; only Claude Code has real-run evidence.
- **Claude first-run keys are observed, not documented.** SpaceMan seeds `.claude.json` keys seen in Claude Code
  2.1.316 (ADR-0021); a Claude Code upgrade must be re-checked with the opt-in real suite.

## Usage and transcripts
- **Codex usage is unknown.** Its records are not verified, so Codex runs show unknown usage (never zero).
- **Cost figures are the harness's own estimates** at list prices; SpaceMan has no price list and cannot enforce
  spending limits from usage that arrives after the fact. Budgets (ADR-0036) warn and stop new runs once reported usage
  passes them; runs already started can go past a budget, and runs with unknown usage are not counted.

## Limits and schedules
- **Budgets are windows over runs started in the last 24 hours**, not calendar days or billing periods.
- **Node capacity bounds worker and diagnostic runs only.** Control, KB support and dreamer runs have their own pools,
  so a node can run more processes than its capacity setting.
- **Throttled event ingestion can cause JetStream redeliveries**: events prefetched by the SDK may pass their
  acknowledgement timeout while waiting; they are deduplicated and recorded as redeliveries, never lost.
- **Delivery attempts are counted from SpaceMan's delivery history** (the Pulse SDK does not expose JetStream's
  delivery count), so history removed by retention restarts the count.
- **Schedules resolve to the minute** (cron) or second (interval), handle at most 10,000 missed occurrences per gap
  individually (beyond that the gap is summarized), and there is no "run now" action yet.
- **No Terraform resources for limits and schedules yet** (ADR-0035 phase 2).
- **Transcripts are uploaded after a run ends**, up to 64 MiB (longer ones keep their start).

## Dreams
- **Token budgets are advisory.** The dreamer is told its token budget; SpaceMan enforces only the time budget (by
  stopping the run) and the evidence budget.
- **Transcripts are found where harnesses keep them** (Claude Code `projects/`, Codex `sessions/` in each instance's
  folder); other formats are not offered.
- **Evidence is copied** into the dream's folder on the node; it is as sensitive as the transcripts.

## Knowledge bases
- **Sessions are per agent instance.** A relaunched worker with a new instance gets a new session.
- **Resumed conversations depend on the harness.** A session woken from sleep continues its conversation where the
  harness supports resuming (Claude Code); otherwise it starts fresh with the same worktree.
- **The knowledge branch must not be checked out** where SpaceMan should move it; acceptance refuses such a branch.
- **The check runs with the node user's rights**, like agent programs.
- **No proposal review by agents.** Accepting is a person's decision (reviewed) or automatic after the check
  (serialized); there is no maintainer-agent workflow.

## Role memory
- **Checkpoints need an idle signal or a run end.** A harness that never reports idle (OSC 7501) is checkpointed only
  when its run ends.
- **Codex may not be able to write memory.** Codex runs get `$SPACEMAN_MEMORY_DIR`, but whether its sandbox lets it
  write outside the workspace is unverified.
- **Central needs git** to store memory, and syncs are limited to 2 MiB of new history at a time.
- **Restored or relocated memory is not merged automatically.** History central did not acknowledge is preserved and
  reported. A relocation flushes and syncs memory first; with an unavailable source, history central never received is
  lost (the acknowledgement says so).
- **No flush before backups.** A backup holds the history central acknowledged.

## Space relocation
- **Only explicit and only stopped spaces.** Nothing moves a space on its own; a space with active runs needs `stop_runs`.
- **Both nodes need protocol 1.3** (this release's agent). An older agent rejects relocation steps, which block.
- **Unpushed is judged locally.** The source compares branches with the remote-tracking refs it has and does not fetch, so
  a branch pushed from elsewhere after the last fetch can look unpushed (it is then refused or exported, never lost).
- **Exports have limits.** 4 GiB per relocation; ignored files are not carried; staged and unstaged changes arrive as
  unstaged changes; worktrees outside the space folder and linked worktrees of outside repositories block the check;
  submodule work trees are inspected only as part of their parent's status.
- **Credentials in remote URLs are dropped.** The destination clones with its own Git authentication; a remote that only
  works with credentials embedded in its URL fails to clone (the relocation blocks with the error).
- **Native sessions are not carried.** Runs on the destination start fresh harness sessions (project continuity only).
- **Without the source, only central's copies.** The unavailable-source flow restores acknowledged memory and the
  repositories the operator lists; everything else on the source may be lost.
- **No CLI.** Relocation is in the UI, the API and Terraform; a `spaceman space relocate` command waits for the CLI work (M5-02).
- **Retention by timer runs only in `spaceman serve`.** Expired retained folders are deleted by central's relocation timer;
  the source must be connected to delete them. A source that never returns keeps its folder.
- **Simulated evidence only.** Two in-process nodes with a fake runner and real Git; no relocation between real machines
  is recorded.

## MCP gateway
- **Streamable HTTP servers only.** Servers started as local processes (stdio) and the older SSE transport are not
  supported; run such a server behind a streamable HTTP adapter.
- **Static headers only.** Upstream OAuth flows are not supported; use a token in a header.
- **Tools are listed at harness start.** A tool or grant added later reaches runs launched afterwards. Revocations and
  disabling apply to the next call.
- **No requests from servers.** Sampling, elicitation, roots and server notifications are not relayed.
- **Upstream state is per run.** A relaunched run, or a run whose session failed or idled out, starts a new session.
- **Approvals for gateway calls** are not enforced yet; they arrive with the approval gateway (M3-05).
- **Shell access bypasses the gateway.** Agents with shell access can reach the network directly from their node.

## Skills
- **Codex runs get no skills.** Codex's skill loading has not been verified, so the node logs a warning and starts the
  run without them. Claude Code runs get them in `$CLAUDE_CONFIG_DIR/skills`.
- **Skills follow their latest version.** The UI assigns skills following the latest published version; pinning a
  version is available only through the API.
- **Built-in skills are seeded once.** A newer SpaceMan with an updated HallPass skill does not change the catalog
  copy; publish the new content as a new version by hand.

## Human actions
- **Approvals are advisory unless a policy gates the operation.** Enforcement covers only operations SpaceMan performs:
  built-in agent tools and gateway tools. Shell commands and a harness's own permission prompts are not intercepted.
- **Agents call again after approval.** A gated call returns at once with a request; the agent must repeat the call
  with the same arguments after the notice. An approval belongs to the run that asked, so a relaunched run asks again.
- **Interrupted executions are not retried.** If central stops while an approved operation runs, its outcome is
  unknown; it is marked interrupted and needs a new approval to run again.
- **No routing when the requester is gone.** A response for a run that ended waits as a stored notification; routing
  it to control or a successor arrives with M4-07 (SM-HUM-009).

## Control and repositories
- **No repository tracking yet.** SpaceMan records each worker's branch and worktree, but not remotes, dirty state or
  unpushed commits (SM-REPO-003). Worktrees are never cleaned up automatically; remove them with git when done.
- **Restart budget is per space and hour.** Three supervisor restarts per hour; the counter is durable (runs created
  by the supervisor), not configurable yet.

## Messaging
- **Terminal notices need an idle program.** Notices are typed only when the harness reports idle or done through
  OSC 7501; a program that never reports status gets no notices and must call `spaceman_messages` itself.
- **Native harness messaging is not used.** Claude Code's and Codex's own messaging features are not verified across
  standalone runs, so SpaceMan does not rely on them (SM-COMM-003).
- **Reminders and escalation arrive in M4**; expiry runs once a minute.
- **Schema forms cover common fields.** Strings (with formats and enums), numbers, booleans and string lists render as
  fields; nested objects and other arrays use the JSON editor.

## Sign-in (OIDC)
- **No real Porch evidence yet.** OIDC sign-in, group mappings, linking and step-up are tested against an in-process
  provider that follows the Porch contract (`porch.groups`, RFC 9207 `iss`, PKCE S256), not against a running Porch
  (SM-AUTH-002).
- **Password sign-in cannot be turned off.** Hallpass's SSO-only enforcement and "sign in with this provider
  directly" setting are not ported (ADR-0029).
- **No provider sign-out.** Signing out ends the SpaceMan session only; RP-initiated logout at the provider is not
  implemented.
- **SSO grants change at sign-in.** A group removed at the provider takes effect at the user's next SSO sign-in, not
  in sessions that are already open (deleting the provider or unlinking applies at once).
- **Step-up leaves the page.** Re-authenticating an SSO session goes to the provider and back; the action that asked
  for it has to be repeated.
- **Test-login previews are in memory** for ten minutes and are lost when central restarts.

## Backups and restore
- **The KEK is the single point of recovery.** Every backup file is sealed under the KEK, and the KEK is never in a
  backup. Losing it loses every backup (SM-STORE-004, ADR-0031). The sealed format (`SMBAK1`) has not had external
  cryptographic review.
- **Node state is not backed up.** Dirty workspaces and worktrees, memory not yet synced to central, node credentials,
  journals and transcripts still on nodes are outside central's backups (SM-STORE-004).
- **A restore is a step back in time.** Nodes enrolled, users and tokens created, and revocations made after the backup
  are not in it: re-enroll such nodes and review users and tokens. Approvals approved with uses left are revoked and
  must be requested again (ADR-0031).
- **Replays and duplicates after a restore.** Event subscriptions replay from their oldest stored delivery (bounded by
  delivery retention); events that arrived after the backup create their inbox items again, and an event nak'd for
  longer than the delivery retention before the backup could be outside the replay. Outbox events pending at backup
  time are published again; consumers must deduplicate by event ID beyond Pulse's 2-minute window.
- **S3 limits.** Each backup object is a single PUT (at most 5 GiB, so a database or file larger than that cannot go to
  S3). The S3 target is tested only against an in-process fake bucket; no run against real S3 or MinIO is recorded.
- **No download from the UI or API.** Backups are fetched from their target (directory or bucket); restore is CLI-only on
  a stopped central.
- **Restore needs `git`** when the backup holds role memory, and attachments outside the data directory are restored to
  the configured `artifacts.local_dir` only.

## Packaging and deployment
- **Linux only, and not yet run on a real Linux host.** The production target is Linux on amd64 and arm64 (SM-PKG-003).
  macOS builds are labelled unsupported. Both systemd units passed `systemd-analyze verify` (systemd 257, in a Debian 13
  container), and the binaries ran inside Linux containers. Neither unit has started a real installation under systemd.
  The release workflow has not run, and no release has been published.
- **No image is agent-ready.** Neither the `spaceman` nor the `spaceman-agent` image contains herdr-core or a harness.
  SpaceMan does not bundle them, because their redistribution terms are unverified (ADR-0034). A containerized node
  needs a runtime image that the operator builds. herdr-core, Claude Code and Codex have not been run inside a container.
- **No images are published.** Operators build them with `make image` and `make image-agent`. Debian package versions
  in the images are not pinned, so rebuilds are not byte-identical. The release SBOMs cover the binaries' Go modules
  and the source tree (including the UI's npm lock), not the images' Debian packages. SBOMs are produced only when
  `syft` is installed.
- **The central image has a shell.** Central needs `git`, so the image is Debian slim rather than distroless (ADR-0034).
- **Restarting the agent unit ends the node's runs.** `spaceman-agent.service` uses `KillMode=control-group`. The
  agent's adoption of live runs after its own restart therefore does not apply under it. The alternative
  (`KillMode=process`, documented in the unit) is untested.
- **The agent's sandbox is looser than central's.** It allows JIT memory, namespaces, `ptrace` and the full `/proc`
  for the harnesses' tooling (ADR-0034). It does not separate spaces on the same node from each other (SM-INV-007).
- **Accepted but unused agent settings.** `management_listen` and `harness.git_binary` in the agent configuration are
  validated but have no effect. The agent serves no health endpoint, and it looks up `git` on `PATH`.
- **Upgrades are unexercised.** No upgrade from one release to another has run. Central and nodes on different versions
  are untested.

## Terraform provider
- **Phase 1 only, not published.** `terraform-provider-spaceman/` covers what the API supports today. Limits, schedules,
  cross-space configuration, space repositories, the backup target and schedule, and users with their grants
  are not resources yet (SM-API-004, ADR-0035). Terraform 1.11 or later is required (write-only arguments).
- **Relocation through Terraform is the clean flow only.** A `node_id` change runs SpaceMan's relocation and waits; a
  refusal fails the apply. The unavailable-source flow needs an operator's acknowledgement in the UI or API. A completed
  relocation through Terraform is tested only against a fake API (the acceptance harness runs no node agents).
- **Removed fields keep their value.** Optional fields left out of the configuration are not owned, so removing one keeps
  the last value instead of resetting it.
- **Some objects cannot be deleted.** Destroying a prompt, skill, event template, human-action type, harness profile or
  role template only removes it from Terraform's state; knowledge bases and dream configurations are disabled; spaces are
  archived unless `purge_on_destroy` is set. Recreating such an object with the same slug fails until it is imported.
- **Write-only rotation is explicit.** Terraform cannot see a changed write-only secret: bump its `_wo_version`.
- **Tested against a local central only** (simulated, `make tf-acc`); no run against a production deployment, a real
  identity provider or a real Pulse deployment is recorded.

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). Verified end to end on macOS with hc 0.1.16 (`test/real` `TestRealNodeEndToEnd`:
  a `SIGKILL`ed agent's run is ended by the guard); not yet verified on Linux.
- **Terminal byte stream has no sequence numbers** (herdr-core 0.1.16). After the node reattaches, SpaceMan records an explicit gap and the new screen repaint. It cannot recover output that was emitted while it was detached.
