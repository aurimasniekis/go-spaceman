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
- **At-least-once events.** Durable event ingestion is at-least-once, with deduplication on the event ID. Core NATS subscriptions are at-most-once and non-durable (SM-EVT-011, ADR-0042).
- **No hard currency limits.** Usage data may be delayed or missing. Unknown usage is shown as unknown (SM-REC-005, SM-LIMIT-003).
- **Single active central instance.** No high availability. Recovery is restore-from-backup (ADR-0001).
- **No infrastructure provisioning.** SpaceMan never creates machines, containers, networks or cloud resources. Operators
  install `spaceman-agent` and its harnesses on machines they own and enroll each node (SM-INV-004).
- **Only SpaceMan's own runs are managed.** A harness or other program started from an agent's shell, or by hand on a
  node, is not a managed worker: it has no run, lease, run credential or recording, and SpaceMan cannot prevent it
  (SM-RUN-003, SM-INV-007).

## Event ingestion
- **Pulse connections only.** Connections use the go-pulse SDK with Pulse `.creds` (a client tag is required), also for
  core NATS subscriptions; a plain NATS server without Pulse-issued credentials cannot be connected. Direct GitHub
  webhooks are not implemented (SM-EVT-019).
- **Core NATS subscriptions lose messages by design.** No replay, redelivery or cursor; messages are lost while SpaceMan
  is down or disconnected, after a storage failure, when the client drops them as a slow consumer, and over the
  ingestion rate. Subjects must still be Pulse subjects under `evt.`. Without a `Nats-Msg-Id`, messages are
  deduplicated by a content hash, so identical messages without an ID or `ce-time` count once (ADR-0042).
- **Lag is pending counts, not time.** `spaceman_ingest_consumer_pending` is the number of messages a JetStream consumer
  has not delivered, summed over subscriptions and refreshed every 30 seconds; there is no per-subscription or
  age-of-oldest lag metric (labels stay bounded).
- **Grants are the Pulse operator's job.** Each subscription needs exactly the durable grant its page shows
  (`durable:<subject>:<durable>`, or `subscribe:<subject>` for core NATS). A missing durable grant shows up as a timeout
  in the subscription's error, not as a clear permission error (go-pulse v0.1.3 behaviour); a missing subscribe grant
  is reported as such. The in-process test server models only subscribe permissions, not Pulse's grants.
- **Live go-pulse-github evidence uses its own fixtures and no GitHub account.** A running pulse-github v0.1.0 published
  its `issues_opened` and `push` webhook fixtures to a real pulsed (`make test-real-pulse`). Its GitHub App
  installation lookup was answered by a local stand-in serving pulse-github's own test JSON, so the real GitHub API was
  not involved. There is no upstream pull request fixture, so the PR template has no real evidence.
- **Paused spaces always ingest, and wakes are not deferred.** Subscriptions keep running while a space is paused
  (there is no setting to stop them), and nothing holds back control's notification of a new item: with **Hold**,
  a control run that is still running is notified at once. Only launches are refused while paused (SM-EVT-014).
- **Custom subscriptions are created without the UI.** The UI subscribes a space to a template; a subscription with a
  custom definition is created through the API or `spaceman subscription create --custom`. Template definitions are
  edited as JSON, not in a visual editor (SM-EVT-017).

## Event publication
- **Off by default; nothing is back-filled.** Events of a period with `events.outbound.enabled: false` are never
  published, and rows left pending when publication is turned off are dropped after the retention (ADR-0030).
- **At least once, no global order.** Consumers must deduplicate on `Nats-Msg-Id`; JetStream deduplicates retries only
  within its 2-minute window. Events of different subjects can arrive in any order (SM-OBS-004).
- **Loop guard is per installation.** An installation drops only events carrying its own `spaceman-origin`. Automation
  depth across installations (an event creating an item, a task and another event) is not bounded yet (SM-EVT-016).
- **No interrupted-execution event.** Approval executions marked interrupted by a central restart are not published.
- **Real Pulse evidence is local only.** Publication ran against a real `pulsed` v0.1.3 with nats-server 2.15.1 in its
  development profile on loopback (`make test-real-pulse`), with a client granted `publish:evt.spaceman.>`. pulsed's
  production profile (TLS behind a proxy) has not been run.

## Accounts
- **No Codex ChatGPT login.** Codex runs use an OpenAI API key; ChatGPT-plan logins (`auth.json`) are not supported yet (K-9).
- **No real Codex run yet.** The Codex adapter is tested with fakes; only Claude Code has real-run evidence. codex-cli
  0.166.0 is installed on the development machine, but there is no Codex or OpenAI credential to run it with.
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
- **Terraform manages limits and schedules** (`spaceman_space_limits`, `spaceman_space_queue_policy`,
  `spaceman_schedule`, ADR-0041); the queue policy has no `If-Match` in the API (see Terraform provider below).
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
- **The flush before a backup is best effort.** Before each backup central asks the connected nodes to checkpoint and
  sync role memory and waits at most a minute (ADR-0045). A space whose node is disconnected, runs an agent older than
  protocol minor 5, does not answer in time or has history central did not acknowledge is backed up with the history
  central acknowledged, and the manifest and the Backups page name it as possibly stale. Offline backups
  (`spaceman backup create` with central stopped) cannot flush (SM-MEM-005).

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
- **Without the source, only central's copies.** The unavailable-source flow restores acknowledged memory, the
  space's configured repositories that have a URL and the repositories the operator lists; everything else on the
  source may be lost.
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
- **Approvals for gateway calls need a policy.** A gateway call waits for a person only when an approval policy covers
  its operation (`mcp.<server>.<tool>` or a prefix such as `mcp.deploys.*`); other calls run at once (ADR-0024).
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
- **Ended requesters are not relaunched.** A response for a run that ended is routed to a live successor run, control
  or the operators, or held for the next run of the same instance; SpaceMan never relaunches the ended run to deliver
  it (ADR-0032, SM-HUM-009).
- **Responses never become inbox items.** A routed response reaches control as a durable notification, not as an inbox
  item; the inbox's `human_response` source kind has no producer (SM-INBOX-001).
- **No search and no clarification requests.** Human actions are listed by type and status only; there is no text
  search, and no way to ask the requester for more information other than a comment (SM-HUM-006).
- **Few filters.** The Needs you page filters by type and by reminded or escalated, and a space's tab by pending or
  not. There is no assigned-to-me, unassigned, space, status, priority or due filter, and no choice of sort order
  (SM-HUI-001).
- **No notification preferences**, and a task's human-action blocker and an action's requester run are shown by ID,
  not linked (SM-HUI-006).
- **Retention deletes whole actions, daily.** A finished action is deleted with its comments and notifications once
  its space's `retention_human_actions_days` has passed, by one daily pass; there is no archive tier and no per-type
  period. Open actions and finished ones that open work still refers to are kept (ADR-0045, SM-HUI-007).
- **Exports are plaintext and whole-space.** A space export holds every human action with its response; there is no
  per-type, per-period or per-requester export, and the archive is not encrypted (ADR-0045).

## Control and repositories
- **Repository reports follow events, not a clock.** The node reports a space's configured repositories (remote,
  default branch, last fetch, changes, unpushed commits, worktrees) when a repository is added or edited, when the node
  connects, when a run of the space ends and on **Sync now**; nothing polls idle spaces (SM-ARCH-004). A report can be
  minutes old and says when it was made; work an agent is doing right now shows after its run ends or the next sync
  (SM-REPO-003). Each report asks every remote for its default branch (`git ls-remote`, 30 s each), so an unreachable
  remote slows the report down. Worktrees are never cleaned up automatically; remove them with git when done.
- **Reassigning a task is API-only.** A task's `assignee_run` and `assignee_role` can be changed through the API (the
  CLI sets the role only); the UI has no reassign control (SM-CTRL-004).
- **The incident policy is partial.** It sets insertion, preemption (running tasks return to ready), the number of
  active top-level tasks and whether escalation reorders. It does not define which triggers may raise incidents
  (operators and control create incident tasks), what happens to a preempted task's workers (they keep running, and
  control is not told), or how preempted work resumes (SM-QUEUE-009).
- **Repository credentials stay on the node.** Configured repositories are cloned and fetched with the node's own Git
  authentication (SSH keys, credential helpers, Hallpass Git roles). SpaceMan stores no Git credentials and refuses
  URLs that carry them, so a remote that needs a token in its URL cannot be configured; set up the node's Git instead
  (SM-REPO-005, ADR-0044). Production accepts `https://`, `ssh://` and scp-like URLs only; `http://`, `git://`,
  `file://` and local paths need development mode.
- **Unlisted folders still work.** Workers may still use a repository folder that is not in the space's list; such a
  folder is reported as unlisted but not tracked, and a relocation carries it only as the source has it. A space has at
  most 50 repositories. A repository's name and folder are fixed; changing the folder means adding another repository
  (the old folder stays on the node).
- **Older agents get no repository syncs.** Configured repositories need node protocol 1.4 (this release's agent); with
  an older agent the configuration is kept, nothing is cloned or reported, and an explicit sync says to upgrade.
- **Retention covers recordings, transcripts, attachments and finished human actions.** Each has a period per space
  (limits, ADR-0045). Inbox items, tasks and messages themselves are kept until the space is purged (only their
  attachments expire); event deliveries and outbound events follow installation-wide retention; audit retention is
  installation-wide. A space's storage quota still bounds new uploads and deletes nothing (SM-SPACE-001).
- **Restart budget is per space and hour.** Three supervisor restarts per hour; the counter is durable (runs created
  by the supervisor), not configurable yet.
- **Repository-local Git programs are refused.** The node daemon refuses to run Git in a repository whose own
  `.git/config` would make Git run a program: filter, merge or diff drivers, SSH or proxy commands, credential
  helpers, includes, URL rewrites. This covers memory checkouts, KB and writer worktrees, and relocation (ADR-0039). A
  repository with Git LFS installed per repository (`git lfs install --local`) is therefore refused. Install LFS in
  the node's global Git configuration instead. Hooks never run for the daemon. Agents' own Git use inside their
  terminal is not affected.

## Recordings
- **A full disk on the node ends the local recording.** The run is unaffected. Central's copy keeps everything shipped
  before the failure and ends with a `trunc` record (`disk`) and the run's close record. Output after the failure is
  not recorded. It is not resumed when space frees up, only when a new attachment starts (for example after an agent
  restart), which leaves a gap.
- **A write failure on central's own disk** stops central's copy of that recording, and nothing marks it there. The
  node keeps its local recording. Tested: node-side only (`make test-fault`).
- **Retention deletes central's copy only.** `retention_recordings_days` deletes central's copy of an ended run's
  recording; the node's local recordings are never pruned by SpaceMan (remove old `runs/<id>/recording` folders on the
  node yourself). The recording size limit (`recording_quota_bytes`) reaches nodes at launch: an agent older than this
  release ignores it and keeps 512 MiB, and a run keeps the size it started with (SM-REC-003).

## Web UI
- **No single attention summary.** A space's overview shows its node, lease, active runs, accounts, roles and usage, and
  the section links count new inbox items, open human actions, operator messages and incoming cross-space requests.
  The next runnable task and skipped tasks with their blockers are on the Queue page, ingestion problems (subscription
  errors, quarantine) on the Events page, memory sync state and conflicts on the Memory page, and knowledge-base work on
  the Knowledge page. Nothing gathers them on one view. Of storage problems, the UI shows only a truncated recording on the
  run's terminal, labelled "Truncated (quota)" even when the node's disk was full; quota use and free space are not
  shown (SM-UI-003).
- **No archive control.** The UI creates, edits, pauses and resumes spaces; archiving, restoring and purging are done
  with the CLI or the API (SM-SPACE-002).
- **No terminal takeover.** Terminals are watch-only. The `terminal:takeover` permission exists, but no API or page
  takes over a terminal; operators give input through **Message the agent** (SM-UI-004).
- **TanStack Table and Store are not used.** They are dependencies, but tables are Mantine tables and state is React
  state and TanStack Query (SM-UI-001).
- **Not every form uses TanStack Form.** Sign-in, setup, reset and recovery, account, spaces, runs, nodes, users,
  tokens, accounts, sign-in providers, inbox items, tasks, event connections and subscriptions, schedules, messages,
  new prompts, and human-action requests, responses and policies do. Other forms (among them MCP servers, approval
  policies, skills, roles and space roles, prompt drafts, event templates, knowledge bases, dreams, cross-space
  settings, relocation, limits, human-action types, group mappings and linked identities) and the schema-driven fields
  inside the response form use plain React state (SM-ATT-007).

## Attachments
- **Only inbox items carry attachments.** Tasks, human actions, comments and messages hold Markdown text only, so files
  can be pasted, dropped or attached only when writing an inbox item, not when replying (SM-ATT-001, SM-ATT-002).
- **Attachment retention follows references, not Markdown.** An attachment expires once every item that references
  it (an inbox item's attachment list) is finished. A `spaceman://attachment/…` link pasted into another item's text is
  not a reference: that link breaks when the attachment expires (SM-ATT-006).
- **Exports exclude recordings and transcripts**, and an export is not encrypted (ADR-0045). Download recordings and
  transcripts per run.
- **Agents fetch attachments through `spaceman_attachment_get`** (text, or base64 up to 512 KiB); they are not copied
  into the run's folder (SM-ATT-005).

## Audit and logs
- **Audit retention trusts its own checkpoint.** After `retention.audit_days` pruned events, verification starts from
  the recorded hash of the last pruned event. Someone able to rewrite the database could also rewrite that checkpoint;
  this is the same boundary as the whole chain (THREAT_MODEL). There is no export of the pruned events: copy the log
  forward (`GET /audit?after=`) before they are pruned (SM-OBS-003).
- **The audit page has no forward view.** The UI pages newest first; the forward cursor is in the API and CLI.
- **No HTTP access log.** Central logs errors and component events, not every request; request counts and latencies
  are metrics (SM-OBS-001).

## Messaging
- **Terminal notices need an idle program.** Notices are typed only when the harness reports idle or done through
  OSC 7501; a program that never reports status gets no notices and must call `spaceman_messages` itself.
- **Native harness messaging is not used.** Claude Code's and Codex's own messaging features are not verified across
  standalone runs, so SpaceMan does not rely on them (SM-COMM-003).
- **Schema forms cover common fields.** Strings (with formats and enums), numbers, booleans and string lists render as
  fields; nested objects and other arrays use the JSON editor.

## Cross-space requests
- **Text only.** Attachments, artifacts and files never cross between spaces; a reply is Markdown text (at most 20,000
  characters at the detailed level) and, for knowledge requests, the KB's evidence paths.
- **Disclosure is a size and field limit, not a content filter.** SpaceMan cuts the reply to the level's length and drops
  evidence or reasons, but it cannot tell whether the text itself reveals something; the destination's control writes it,
  and the tool description tells it not to paste secrets.
- **Causation is declared.** Depth and loop protection follow `caused_by` (or the only open incoming request). A control
  that declares a request `independent` starts a new chain; that is audited, and quotas still bound it.
- **Only control asks and answers.** Operators can withdraw outgoing and decline incoming requests, but not create or
  answer them from the UI.
- **Peers are configured by ID.** The UI offers the spaces the operator can see; there is no directory of spaces an
  operator cannot read.
- **No real-harness evidence.** Cross-space settings and peers are managed through the API, the UI and Terraform
  (`spaceman_space_xspace_settings`, `spaceman_xspace_peer`, ADR-0041), tested against the simulated suite only.

## Sign-in (OIDC)
- **Real Porch evidence is local, in development mode.** Sign-in, `porch.groups` mappings, group JIT and step-up ran
  against a real Porch v0.3.0 (`make test-real-porch`, SM-AUTH-002), but Porch ran on `http://localhost` without TLS,
  its upstream was Porch's fake Google test fixture rather than Google, and a scripted browser followed the link behind
  Porch's sign-in button. Linking by invitation and the test login were not part of the real run.
- **Porch must allow the `groups` scope.** A Porch application's allowed scopes default to `openid profile email
  offline_access`; unless `groups` is added, Porch drops the scope silently and SpaceMan sees no groups.
- **SSO-only and direct sign-in are installation-wide.** Password sign-in is turned off for everyone or no one, as in
  Hallpass; there is no per-user SSO-only flag (ADR-0045).
- **Provider sign-out needs no ID token, so Porch keeps its session.** RP-initiated logout sends `client_id` and
  `post_logout_redirect_uri` but no `id_token_hint` (SpaceMan does not keep ID tokens, as in Hallpass). Porch v0.3.0
  ends its own session only with an `id_token_hint`, so against Porch the sign-out redirects back without signing out
  at Porch. Provider sign-out has run only against the in-process test provider (ADR-0045).
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
  S3). The S3 target is verified against MinIO only (`make test-real-s3`: backup, list, verify, tamper detection and
  offline restore over TLS, path-style). AWS S3 and Cloudflare R2, virtual-hosted addressing, listings past 1000 keys,
  and scheduled runs and retention against a real bucket are unverified.
- **No download from the UI or API.** Backups are fetched from their target (directory or bucket); restore is CLI-only on
  a stopped central.
- **Restore needs `git`** when the backup holds role memory, and attachments outside the data directory are restored to
  the configured `artifacts.local_dir` only.

## Packaging and deployment
- **Linux only, and not yet run on a physical or cloud Linux host.** The production target is Linux on amd64 and arm64
  (SM-PKG-003). macOS builds are labelled unsupported. Both units ran a real installation of the linux/arm64 archive under
  systemd 257 in a Debian 13 container (`make test-real-systemd`, Docker Desktop's VM). linux/amd64 has not run under
  systemd, because systemd's own services fail under Docker Desktop's amd64 emulation. The agent unit has not run
  enrolled with herdr-core. Under `Restart=on-failure` an agent that fails at start (for example, hc missing) is
  restarted every 5 s until stopped. The release workflow has not run, and no release has been published.
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
- **Accepted but unused central settings.** `auth.local_login_disabled` and `nodes.command_timeout` are validated but
  have no effect: password sign-in is turned off at run time on the Sign-in providers page or with
  `spaceman oidc settings set --sso-only` (which checks first that an administrator can still sign in), not by
  configuration, and node commands use fixed timeouts. `artifacts.backend: s3` passes validation, but central
  refuses to start with it because the S3 artifact store is not implemented; recordings, attachments and transcripts
  are stored on the local disk. Backups have their own, implemented S3 target. See the
  configuration reference (`site/reference/configuration.md`).
- **Upgrades are unexercised.** No upgrade from one release to another has run. Central and nodes on different versions
  are untested.

## Terraform provider
- **Phases 1 and 2, not published.** `terraform-provider-spaceman/` covers what the API supports today (ADR-0035,
  ADR-0041) and space repositories (ADR-0044). The backup target and schedule (configuration file only), type permissions
  beyond space enablement (no API) and editing a user's email, display name or `mfa_required` (no API) are not managed
  (SM-API-004). Terraform 1.11 or later is required (write-only arguments).
- **No server-side precondition for two resources.** The queue policy API has no version and a user's grant changes do
  not bump the user's version, so `spaceman_space_queue_policy` and `spaceman_user_grants` re-read before writing and
  refuse a change made since the refresh; a change landing between that read and the write is still overwritten.
- **Importing `spaceman_space_limits` takes every stored override.** There is no configuration at import time to tell
  which overrides are Terraform's, so the next plan clears the ones the configuration does not set.
- **Relocation through Terraform is the clean flow only.** A `node_id` change runs SpaceMan's relocation and waits; a
  refusal fails the apply. The unavailable-source flow needs an operator's acknowledgement in the UI or API. A completed
  relocation through Terraform is tested only against a fake API (the acceptance harness runs no node agents).
- **Removed fields keep their value.** Optional fields left out of the configuration are not owned, so removing one keeps
  the last value instead of resetting it. Exceptions: removing a limit from `spaceman_space_limits` clears that override,
  and cross-space peer flags and user grants are owned whole (removing one turns it off).
- **Some objects cannot be deleted.** Destroying a prompt, skill, event template, human-action type, harness profile or
  role template only removes it from Terraform's state; knowledge bases, dream configurations and users are disabled;
  a queue policy is kept and cross-space settings are reset to the defaults; spaces are archived unless
  `purge_on_destroy` is set. Recreating such an object with the same slug (or a user with the same email) fails until it
  is imported.
- **Write-only rotation is explicit.** Terraform cannot see a changed write-only secret: bump its `_wo_version`.
- **Tested against a local central only** (simulated, `make tf-acc`); no Terraform run against a production
  deployment, a real identity provider or a real Pulse deployment is recorded.

## Extension points
- **Not every adapter has a Go interface yet** (SM-ARCH-003). Harnesses (`harness.Harness`, compiled-in `Registry`),
  runners (`runner.Runner`), outbound publication (`outbox.Publisher`), attachment content (`attachments.Blobs`),
  backup targets (`backup.Target`) and the upstream MCP gateway (`agenttools.Upstream`) do. Inbound events are bound to
  the go-pulse SDK in `internal/events`, recordings and transcripts are written straight to central's disk, and the MCP
  gateway speaks only streamable HTTP; none of these has an interface for a second adapter.

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). Verified end to end on macOS with hc 0.1.16 (`test/real` `TestRealNodeEndToEnd`:
  a `SIGKILL`ed agent's run is ended by the guard; rerun 2026-11-03); not yet verified on Linux, because no herdr-core is
  installed in the Linux container the systemd suite uses (ADR-0034).
- **External integrations still without real evidence** (SM-VER-002, re-checked 2026-11-03):
  - a real Codex run: no Codex or OpenAI credential on the development machine;
  - AWS S3 and Cloudflare R2: no account (MinIO only);
  - a physical or cloud Linux host, and linux/amd64 under systemd (Docker Desktop's amd64 emulation breaks systemd);
  - Google behind Porch and the GitHub API behind go-pulse-github: no accounts (Porch's fake Google fixture and a local
    GitHub App stand-in serving pulse-github's own test JSON were used);
  - TLS and production profiles of Porch and pulsed: both ran in development mode on loopback.
- **Terminal byte stream has no sequence numbers** (herdr-core 0.1.16). After the node reattaches, SpaceMan records an explicit gap and the new screen repaint. It cannot recover output that was emitted while it was detached.
