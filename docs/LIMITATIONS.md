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
- **The comment, review and CI templates have no live evidence.** `github-pr-comment`, `github-issue-comment`,
  `github-pr-review`, `github-pr-review-comment`, `github-workflow-run` and `github-check-suite` (ADR-0061) are tested
  against payloads derived from GitHub's documented webhook shapes and pulse-github's normalisation code, not against
  events a live pulse-github published. Lifecycle actions (closed, reopened, ready_for_review, …) keep the existing
  issue and pull request templates' generic rendering, which still puts the issue or pull request title in the item
  title.
- **Paused-space policies cover event items only.** A subscription's `while_paused` (defer, wake or stop, ADR-0049)
  decides about the items it creates. Other inputs of a paused space (operator items, worker messages, schedules,
  task and run notices) still reach a control run that runs under **Hold** at once. A `stop` subscription's durable
  events wait only as long as the Pulse stream keeps them; core NATS messages published while it is stopped are lost.
  Deferred notices are sent for items still `new` at the resume, oldest first, at most 10,000 per resume; the rest go
  out at the next resume or central start (SM-EVT-014).
- **Definitions are edited as JSON.** A custom definition (in the subscribe dialog) and a template draft are written as
  JSON text, checked as you type and by the server, with a sample-event preview; there is no field-by-field visual
  editor (SM-EVT-017).

## Event publication
- **Off by default; nothing is back-filled.** Events of a period with `events.outbound.enabled: false` are never
  published, and rows left pending when publication is turned off are dropped after the retention (ADR-0030).
- **At least once, no global order.** Consumers must deduplicate on `Nats-Msg-Id`; JetStream deduplicates retries only
  within its 2-minute window. Events of different subjects can arrive in any order (SM-OBS-004).
- **Automation depth follows recorded links only.** An installation drops events carrying its own `spaceman-origin`,
  and quarantines events whose `spaceman-depth` reaches the space's `event_depth` (default 8, ADR-0049). The depth
  passes from an event to its inbox item, to tasks created from the item (`origin_kind` inbox_item) and their
  subtasks, and to their events. Runs, human actions, approvals, memory and KB events carry no depth: a change control
  makes on its own judgement after reading an item starts a new chain, bounded only by the run, tool-call and budget
  limits. A publisher that is not SpaceMan can send any depth, including none (SM-EVT-016).
- **No interrupted-execution event.** Approval executions marked interrupted by a central restart are not published.
- **Real Pulse evidence is local only.** Publication ran against a real `pulsed` v0.1.3 with nats-server 2.15.1 in its
  development profile on loopback (`make test-real-pulse`), with a client granted `publish:evt.spaceman.>`. pulsed's
  production profile (TLS behind a proxy) has not been run.

## Accounts
- **Codex API keys are stored on the node.** Interactive Codex reads an API key only from a stored login, so the node
  keeps it in each Codex instance's `CODEX_HOME/auth.json` (0600) between runs, rewritten at every launch; it is not
  removed when the account is deleted (ADR-0054). Claude Code credentials still reach a run only through the launcher.
- **A Codex ChatGPT login belongs to one node.** The node keeps one live copy of each ChatGPT login, which Codex
  refreshes in place; a refresh token works once, so the same login used on two nodes stops working on one of them
  after the first refresh. The copy stays on the node (0600, readable by the agent's user and root) and is not removed
  when the account is deleted. A real ChatGPT login has not been used: the layout was verified with codex-cli 0.166.0
  using an API-key file (ADR-0053, SM-ACCT-002).
- **No real Codex run on an OpenAI account yet.** There is no Codex or OpenAI credential to run with. Real codex-cli
  0.166.0 runs through SpaceMan were made only against Harbor Pilot `serve --demo` with synthetic upstreams
  (`TestRealHarborPilotGatewayRuns`, 2026-11-08). They show the launch through the shim, a model turn and a second
  prompt by safe input, but not an API-key or ChatGPT login.
- **Claude first-run keys are observed, not documented.** SpaceMan seeds `.claude.json` keys seen in Claude Code
  2.1.316 (ADR-0021); a Claude Code upgrade must be re-checked with the opt-in real suite.

## Harness distribution
- **Managed Claude Code is opt-in.** With `harness.claude.manage` off (the default) a run executes whatever `claude`
  its `PATH` resolves to, and an unpinned run records no version. A profile pin on such a node runs only when it equals
  the detected version, and a channel pin (`latest`, `stable`) fails there.
- **A node's reported version can lag by up to 30 minutes.** From protocol 1.11 a connected node re-reports its
  harness versions when its Claude Code cache or a channel target changes and at least every 30 minutes (SM-HARN-008);
  an unmanaged `claude` updated by hand shows up at the next periodic report. Older agents report only when they
  connect. The manifest's `node_default` is the last report; the run row holds what actually ran.
- **Pins need protocol 1.11.** Central refuses to send a pinned launch to an older node agent: the run fails as
  `node_too_old`. A node learns pins only from launches it has seen, so GC may remove a pinned version that no launch
  has used for 30 days. The next pinned launch downloads it again.
- **Checksum-verified, not signature-verified.** Under ADR-0077 the node accepts a downloaded Claude Code binary when
  its SHA-256 and size match the version's `manifest.json`, fetched over HTTPS from the release host or the operator's
  mirror. The manifest's signature is not checked until the release key and procedure are documented (K-14), so
  whoever controls that host or mirror controls the binary.
- **Live runs keep their version.** Under ADR-0077 a new version reaches new runs only; a running conversation moves
  only if it is stopped and resumed. Resuming across versions was checked once, 2.1.317 to 2.1.322 in print mode
  against a fake API (`docs/contracts/harnesses.md`, Distribution).

## Harbor Pilot gateway
- **Gateway runs need updated agents.** A node agent below protocol minor 9 can't hand the token to a harness, so
  central fails a routed launch to it as `agent_too_old` before minting, rather than let the harness start on the
  node's own login (ADR-0060). Update every node of a space before opting the space in.
- **A Harbor Pilot outage stops the space's gateway runs.** There is no fallback to a direct account (Q1): when
  Harbor Pilot is down, or its parent token is revoked or expired, every launch of a routed harness fails (at mint, as
  `gateway_unavailable`), and running runs lose model access. Revoking the parent in Harbor Pilot is the emergency stop
  for every gateway run at once (THREAT_MODEL T-39), and for the same reason a single point of failure for every
  opted-in space. Recovery is a new parent saved on the connection; runs that lost access must be relaunched.
- **No token refresh inside a run.** A run's token lives for the space's token lifetime (24 h by default, 15 min to
  7 days, Q2). A harness can't be given a new token while it runs, so a run that outlives its token loses model
  access and must be relaunched. A re-sent launch gets a fresh token only before its harness starts.
- **Codex's default model may not be routed.** A gateway Codex run without a model uses Codex's own default
  (`gpt-6.1-sol` in 0.166.0), which a Harbor Pilot plan may not route: the demo answers 403 "no eligible account". Set
  the role's model, or the space's allowed models, to one the plan routes. SpaceMan doesn't pick one.
- **Repository and harness-written settings are not checked.** In a gateway run SpaceMan refuses Claude Code profile
  settings that add a credential or endpoint, and removes stored logins. It does not inspect a Claude Code project
  `.claude/settings.json` in the run's working directory (repository content), keys Claude Code or the agent wrote
  into the instance's own `settings.json`, or a Codex `config.toml` in `CODEX_HOME`. Whether a project file's `env`
  can redirect Claude Code's base URL was not tested. The same holds for direct accounts (THREAT_MODEL T-36). A Codex
  `config.toml` can't redirect a gateway run's model traffic: with codex-cli 0.166.0 the `-c model_provider…`
  overrides won over its root `model_provider`, over a `<name>.config.toml` profile (even with `--profile`, which
  SpaceMan never passes) and over its own `[model_providers.harbor-pilot]`, and a legacy `profile =` key makes Codex
  refuse to start (checked by hand against two local servers, 2026-11-08). It can still add settings the overrides
  don't set, such as extra request headers. Later Codex versions may change this precedence.
- **The run can read its own token.** A gateway run's token is in its harness's environment, as account credentials
  are. It is bound to the run in Harbor Pilot and revoked when the run ends, but stays usable until then.
- **A crash right after a mint leaves an untracked token.** If central stops between Harbor Pilot's answer to a mint
  and the transaction that stores the token, nothing records that child token. It stays valid until its TTL (the
  space's token lifetime, 24 h by default) unless it, or the parent, is revoked in Harbor Pilot.
- **Revocations and the lease-loss sweep run only in `spaceman serve`.** They run on serve's deadline timer. A
  revocation queued by another process (an admin command) waits in the durable queue until the next `serve` runs.
- **Failed revocations need a person.** A revocation Harbor Pilot refuses, or one whose parent was replaced before it
  could be done, is audited, counted (connection page, `spaceman_gateway_revocations_failed`) and left to a person;
  no human action is raised.
- **The 0.1.3 minimum is still checked by a feature probe.** A gateway that does not know `check_budget` on mint
  (before 0.1.3) is refused; a later gateway that dropped the field would look old too. Harbor Pilot 0.1.4's
  `GET /api/v1/system` is read only with the service-principal token (it needs `config.read`), so without one the
  version shows as unknown and budget status, when it can be read at all, takes the 0.1.3 path. SpaceMan never compares
  version numbers: a gateway that answers `/api/v1/system` is treated as 0.1.4 or later.
- **Budget status by project only on 0.1.4 or later.** On 0.1.3 SpaceMan still reads every budget (the list is not
  paginated) and looks the project's workspace up. A 0.1.4 gateway whose version can't be read for a moment (a failed
  `/api/v1/system` read is retried after 5 s) is read the 0.1.3 way meanwhile. The version is cached for 10 minutes:
  when the gateway at the same address is downgraded below 0.1.4 meanwhile, it ignores the filters and answers every
  budget. SpaceMan takes an answer holding another project's or binding's budget, an account budget, two workspaces' or
  an item without `remaining_e10` (which 0.1.4 always sends) as unfiltered, drops the cached version and reads every
  budget the 0.1.3 way. A gateway that sends `remaining_e10` but ignores the filters would not be detected when its
  only foreign budget is one other workspace's. A filtered read that answers 404 (the space's project or the binding
  was deleted in Harbor Pilot) also falls back to the 0.1.3 path.
- **The launch check waits at most 15 s.** Every Harbor Pilot read of one launch's budget check (version, budget
  status, a fallback) shares one 15 s deadline; when it passes, status counts as unavailable and the space's
  `on_budget_status_unavailable` decides. The read itself goes on in the background and is cached for the next launch.
- **Inactive and unknown budget states never refuse.** Scheduled, expired, disabled and any state a later Harbor Pilot
  adds are shown but never refuse a launch, so SpaceMan's early check can't refuse more than Harbor Pilot would; Harbor
  Pilot's own mint check and request enforcement still apply.
- **A re-sent budget alert is a new inbox item.** After an upgrade to Harbor Pilot 0.1.4, or after a limit is raised,
  a budget alert that already fired in the current period fires again. SpaceMan makes it one more item in the same
  correlation as the first (only a redelivery of the same event is a duplicate); it does not merge them.
- **Demo evidence only.** Everything real ran against `harbor-pilot serve --demo` 0.1.3 (and 0.1.4 for the
  `make test-harbor-pilot` suite) with its synthetic upstreams:
  connection checks, run tokens, real Claude Code and Codex runs, budgets, per-run usage, events and the Terraform
  example. No run has gone through a production gateway to a real model provider (Q7: the operator will provide one;
  the evidence map is in RELEASE_READINESS §13).
- **Unscoped Harbor Pilot events have no routing decision.** Where quota, blocked-request and non-project budget events
  should go is undecided (Q4, DECISIONS K-12); until it is decided they reach only spaces that subscribe.
- **Harbor Pilot rounds share central's deadline timer.** Revocations, the lease-loss sweep and usage reads run one
  after another on the timer that also expires human actions and approvals and fires schedules. A revocation
  call is bounded by 5 s and a usage read by 10 s. A usage round stops asking Harbor Pilot after its first transport
  failure, but a revocation round still tries each due revocation (at most 10), so a gateway that accepts connections
  and never answers can delay human-action expiry, reminders and schedules by up to about 50 s per round, again each
  time the revocations are retried.
- **The early budget refusal is advisory and up to 30 s old.** SpaceMan reuses a budget status read for 30 s (5 s
  after a failed read), so a budget used up in that window can let a launch through. Harbor Pilot then refuses the
  run's token mint (`check_budget`) or its requests. The check can't see a request in flight that is about to push a
  budget over its limit. A budget with no spend yet passes even when the first request is then refused for its own
  estimate (Harbor Pilot's rule). Budgets on an upstream account are not shown or checked, because they cover
  whichever account serves a request, not a space. Harbor Pilot doesn't check them at mint either.
- **Workspace budgets need `config.read`.** Finding the project's workspace needs the service-principal token's
  `config.read`. It is looked up only when a workspace budget exists, and cached for 10 minutes. If the lookup fails,
  budget status is unavailable for the space, and its `on_budget_status_unavailable` decides.
- **Project usage counts every client.** The Limits card's project usage is Harbor Pilot's usage of the project over all
  its clients, not only this space's runs. Several spaces sharing a project see the same figures.
- **Per-run usage arrives late and may miss a slow request.** A gateway run's usage is read about 15 s, 2 min and 10 min
  after its end. A request still streaming after the last read is not counted. A run that ended while no
  service-principal token was set gets no usage. A read failing for an hour is given up, and the run keeps what it had.
  Usage is never written as zero: a read that finds no request for the run (the ledger behind, a binding changed, a
  run that made no request) stays with the run's gateway figures only, and the run's usage stays unknown, so a gateway
  run that really made no request is shown as unknown, not zero. Harbor Pilot counts requests whose provider reported
  no usage; their tokens are unknown and not counted.
- **Budget status errors on SpaceMan's side follow the unavailable policy.** A service-principal token that can't be
  opened or a database error while reading the connection counts as "status can't be read": the space's
  `on_budget_status_unavailable` decides, and the person sees a fixed reason (details only in central's log).
- **Who may read budget status.** A space's Harbor Pilot status needs only `space_config:read`, and it shows the global
  and client-binding budgets whose spend sums every space (ADR-0060, by design).
- **Status refreshes once a minute in the UI.** The Limits card re-reads status every 60 s (central reuses a read for
  30 s); a failed read shows an error on the card.
- **Only project budgets map to a space.** The `harbor-pilot-budget` template routes a project-scoped budget to the
  space whose subscription names that project. Quota windows, blocked requests and binding, workspace, account or global
  budgets name no project: they reach only spaces that subscribe to them, and nothing routes them automatically (Q4 is
  undecided, DECISIONS K-12). A blocked request is not mapped to the run or space that sent it.
- **One item per refused request.** Harbor Pilot sends `access.request.blocked` for every refused request; refusals
  of one binding share a correlation, but each is its own inbox item. There is no digest.
- **Harbor Pilot events: relay tested against SpaceMan's test Pulse only.** Harbor Pilot's own relay was run into
  SpaceMan's Pulse-shaped JetStream test server, not into a real `pulsed`. The quota window template was never fed a
  live event (the demo emits none) and is tested on events derived from Harbor Pilot's code.
- **A new Harbor Pilot Pulse destination misses earlier events.** Harbor Pilot starts a destination at its newest
  event; anything emitted before the relay picked it up is only sent by a replay from Harbor Pilot.
- **Stricter than Harbor Pilot's own validation.** The templates' input schemas expect Harbor Pilot 0.1.3's id, name
  and amount formats. An event that differs (a forged one, or a later Harbor Pilot that changes a format) is
  quarantined, not shown.

## Usage and transcripts
- **Codex usage is unknown** unless the run went through Harbor Pilot. Its records are not verified, so direct Codex
  runs show unknown usage (never zero). Gateway runs get Harbor Pilot's figures after they end (source `harbor-pilot`).
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

## Usage limits
Holding accounts and parking runs at a Claude usage limit (ADR-0079, SM-RUN-004..006):
- **No live subscription limit has been observed.** A real claude.ai limit can't be forced, and none has been seen
  on a SpaceMan run yet. Every test of the feature is simulated: scripted node reports, fake transcript records in
  the shapes the contract recorded (placeholder values), a fake clock and hpfake. Real evidence covers only the gateway side: the
  quota board and the all-exhausted 429 of a real `harbor-pilot serve --demo` (v0.1.6 and v0.1.4, 2026-11-14), with a
  scripted node and harness. TRACEABILITY's real-evidence column for SM-RUN-004..006 stays empty until a live limit
  is observed (K-16).
- **Not verified with real accounts (K-16):** `--resume` of a real parked session, and `--resume` under a different
  real claude.ai account (failover; only different tokens against a fake API were tried); how a real `--resume` fails
  when its session is missing; the `seven_day_opus` and `seven_day_sonnet` kinds; whether Claude Code reports an
  `error` status when a turn ends at its limit (the node scans on idle, error and end, so detection doesn't depend on
  it).
- **A gateway reset is an estimate.** Harbor Pilot's 429 names no reset, so SpaceMan takes the earliest `until` on
  the quota board, which covers every account of the Harbor Pilot installation, not only those the space's plan routes
  to (it has no project filter), or a blocking budget's period end. A run may resume early, hit the limit again and be
  parked again (no new notice), or resume up to 15 minutes late. Without the service-principal token, or while Harbor
  Pilot names no reset, the hold is probed like any unknown reset.
- **Codex isn't covered.** Only Claude Code transcripts are scanned. A Codex run at its limit is neither kept alive nor
  parked and opens no hold; it behaves as before (control may stop or relaunch it). A Codex launch through a Harbor
  Pilot connection that a Claude Code run found exhausted is refused while that hold is open.
- **Detection follows an undocumented format.** It reads typed fields of Claude Code's transcript records, recorded
  for 2.1.317–2.1.323 (`docs/contracts/harnesses.md`). A renamed field silently stops detection, and runs then behave
  as they did before ADR-0079. Node agents below protocol minor 12 never report limits.
- **More than half of subscription waits have no reset time.** Claude Code's wrap-up path names neither the reset nor
  the kind, so such a hold is probed (one launch after 1 h, then 2 h, 4 h …, at most every keep-alive) and a parked run
  is resumed by that probe, possibly hours after the real reset.
- **A run's transcript can hold its account.** The transcript is untrusted. A forged record can hold an account for at
  most 8 days (the clamp), in every space bound to it; any progress on the account and an operator's clear end it.
- **Model-specific weekly limits hold the whole account**, for every model, not only the exhausted one.
- **KB support and dreamer runs aren't parked**; their schedulers start them again when needed.

### Account switching and pools (ADR-0090)
- **No real two-account evidence.** A move to another claude.ai account, its `--resume` under that account, the
  `oauthAccount` change, whether the status line shows the new account's windows, and whether the `StopFailure` hook
  and the notification fire as documented at a real limit are all unverified. Everything is simulated: a fake
  harness, a fake API, a scripted node and a fake clock.
- **Headroom gate is simulated only.** With `usage_limit_min_headroom_percent` above 0, a new run (a launch, a resume of a
  parked run or a switch's resume) skips an account whose known window leaves less than that share free, and waits for the
  pool's earliest reset (at most an hour: usage older than an hour no longer counts). It reads the status-line windows of
  Claude Code runs, so an account that reports none is never refused, and an unreadable usage ledger fails open.
  Running agents are never moved for it. Tested with fake windows and a fake clock only.
- **Pool order ignores utilisation.** The resolver orders by priority, then the fewest live runs, then binding order. The
  windows are recorded and shown, and the headroom gate can refuse an account, but they don't rank the pool.
- **Claude Code only.** Codex runs are not moved; a Codex run at its limit waits as before.
- **A move ends the old process.** Background shells and subagents end with it, and the new run's prompt cache starts
  cold (an extra uncached request).
- **A move can misattribute the last tokens of the old run.** A report that is lost or late is booked with the next
  run's report (the account after the move); tokens are counted once, but to the wrong account.
- **Subscription accounts are the case covered.** The status line reports windows for subscription accounts; an
  account that reports none shows unknown use.
- **Pool-exhausted notices can be late.** A space at its open-action cap, or a crash in between, raises the notice
  later, within the hour; it is dropped once its window passes, never raised for a reset in
  the past.
- **Web UI not built.** The usage page, the switches list and the pools editor are not in the web UI yet; they are
  available through the API and the CLI.
- **Harbor Pilot routing per role isn't built.** Harbor Pilot is for third-party models only; no role is routed through
  it to pick a cheaper model.

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
- **The memory guard is a guard against accident and against other instances, not a sandbox** (ADR-0025 addendum,
  ADR-0094). The node refuses git in a checkout with a redirected git dir, config-based hooks, links inside `.git`,
  gitlinks, a nested `.git` or a FIFO, and never stages or materialises `.claude/`, `.codex/`, `.agents/` or a top-level
  `.mcp.json`. An agent can still edit its own checkout's `.git`, and the guard is rewritten at every checkpoint and run
  start. A harness-config file an instance writes into its own checkout stays there, unshared, until the checkout is
  next prepared (and then only if the sparse checkout was not already active). Central keeps history it was already
  given, so its file listing can still show such paths. A refused checkout is not repaired by SpaceMan: the operator
  removes or recreates it.
- **Fast-forward touches only idle, clean checkouts** (ADR-0094). A checkout with local changes, a diverged history, a
  run that is working or blocked, a taken-over terminal or input still waiting for its turn is left to its next
  checkpoint, which may still preserve a conflict. `memory.fast_forward: false` turns it off.
- **A role rename can stay open until an admin acts** (ADR-0093, addendum T-H4). A busy role is refused: a rename waits
  while a run of either name (or a dream run in the space) is active on the node, and the move happens when it ends. A
  node that keeps reporting `error` leaves the rename `failed` (central does not queue the command again on an `error`),
  and the node retries an `error` only at its next agent start, not periodically. A node downgraded below protocol minor 14
  after a rename never receives it. Until it is done the rename holds launches of both names and dream launches and
  refuses a clean relocation. The exits: fix or upgrade the node and **retry** (`spaceman role retry-rename`), **close**
  it (`spaceman role close-rename`, admins only), or a `source_unavailable` relocation (which closes the open rename as
  `not_needed`). A close is final: the rename is `abandoned`, and the node's old-name folder is left on the node as an
  orphan, never deleted and never synced again, so anything it held that central never received stays only there (a later
  role of that name sets it aside under `.retained/`). A closed rename whose command the node has not received yet keeps
  refusing renames of both names until the node connects, or until an admin closes it with `--forget-node`, which
  gives up delivery (the node, if it returns, keeps the old folder as an orphan). Retry and close are not in the web UI or Telegram. A deleted role's
  leftover memory collection blocks reusing its name as a rename target. Deleting or archiving a role's memory and a web UI
  rename are follow-ups. A rename changes only its own space: the installation-wide default human policy keeps a successor
  role of the old name.

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
- **The destination gets a keyed repository's key at Prepare.** For a repository with a SpaceMan SSH key, the
  relocation's destination receives the key in Prepare, before the lease moves, so the destination of a cancelled or
  failed relocation has held the key in memory. Nothing is stored on that node.
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

## SpaceMan MCP
- **Simulated evidence only.** Servers are managed through the API, the CLI and the web UI, runs use
  the tools over the agent bridge and the external endpoint is served. All of it is tested in process with the real
  agent and bridge and a fake runner (`TestSpacemanMCPOverBridge`); no real harness has driven the tools through a
  model yet. The Terraform resource is in terraform-provider-spaceman PR #12, not yet released.
- **Full admin by default, by design.** The default preset is `admin` on every space and the token never expires
  (operator decisions for a trusted infra space). An agent with these tools can do what its owner can, short of the
  hard exclusions; prompt injection into that space is accepted risk T-33.
- **In-run approvals are decided in the run's space.** A dangerous call's approval request appears in the space of the
  run that made it, and anyone with `approval:decide` there may approve it, also when the call acts on another space
  or on an installation-wide target. Give `approval:decide` in the infra space only to people trusted with the
  server's reach (accepted risk, T-33).
- **Launches are writes, overrides included.** `launch-run` needs no approval under the default `approval: none`,
  also when it overrides the role's `settings.permission_mode` (up to the installation ceiling) or `account_ref`;
  only diagnostic launches are gated. Saving a space role is dangerous, because it changes every later launch by
  that role (accepted risk, T-33). Set `approval: writes` to gate launches too.
- **Owned by a person.** The token acts for the admin who created the server: if that admin is disabled or loses
  grants, the server stops working until it is re-issued by someone else.
- **External endpoint (`/api/v1/mcp`): on for every user API token.** There is no setting to turn it off, globally
  or per token; a token can do there what it can do on the API. To keep it unreachable, don't forward `/api/v1/mcp`
  from the reverse proxy (accepted risk, T-38).
- **External endpoint (`/api/v1/mcp`): bearer tokens only.** A client must send `Authorization: Bearer
  spaceman_at_…`; there is no OAuth flow, so clients that only support OAuth for remote servers can't connect.
- **External endpoint: no dangerous tools without a second person.** A dangerous call needs an approval from someone
  other than the token's user (that user may only deny or withdraw it), so an installation with one administrator
  can't run them through the endpoint with a user's approval: approve with the root token, or use the API, CLI or
  web UI. Dangerous tools that act on no space (accounts, Git SSH keys, HTTP MCP servers, harness
  profiles, role templates, publishing, installation-wide human policies, node writes) and sensitive reads
  (recordings, transcripts) are not offered there at all.
- **External endpoint: the root token can approve.** An approval the endpoint asked for can be approved with the root
  token; on a single-admin installation that is the documented approval path (operator decision, ADR-0063).
- **External endpoint: one message per request.** JSON-RPC batches and protocol versions before 2025-06-18 are
  refused (400), so the per-token rate limit counts calls.
- **External endpoint: the token's grants are the selection.** There is no per-token tool preset; issue a token with
  narrower grants for a narrower client.

## Skills
- **Skill loading is verified, not a skill's use.** Codex runs get skills in `$CODEX_HOME/skills` (listed in the model's
  prompt input with codex-cli 0.166.0, no model call), Claude Code runs in `$CLAUDE_CONFIG_DIR/skills`; whether a model
  follows a skill is up to the model. Codex also lists its own built-in skills (`.system`), which SpaceMan leaves alone.
- **Skills follow their latest version.** The UI assigns skills following the latest published version; pinning a
  version is available only through the API.
- **Built-in skills are seeded once.** A newer SpaceMan with an updated HallPass skill does not change the catalog
  copy; publish the new content as a new version by hand.

## Human actions
- **Terminal prompts are denied for Claude Code only** (ADR-0066). Claude Code runs never see `AskUserQuestion` or
  `EnterPlanMode`, and every run's orientation says to use `spaceman_human_ask`. A profile gets either tool back by
  listing it in its own `permissions.allow` (per tool; there is no global switch). Codex's `request_user_input` (Plan mode, or Default mode
  with its feature flag) is only discouraged by the prompt: SpaceMan sets no override for it yet, because none has
  been verified against a live Codex session. A harness's own permission dialogs still appear in the terminal.
- **Claude Code permission dialogs are denied, not answered** (ADR-0068). `skipAutoPermissionPrompt` only skips the
  one-time notice shown when auto mode starts. Auto mode still raises a dialog for `ask` rules, for reads outside the
  working directories, and when its classifier falls back to asking after repeated blocks. SpaceMan's
  PermissionRequest hook denies each of them, so an `ask` rule works like a deny in SpaceMan runs: a person can't
  approve the call from the dialog, and the agent has to ask through `spaceman_human_ask` and then find another way or
  wait for a profile change. A profile that sets `disableAllHooks` (or a managed `allowManagedHooksOnly`) turns the
  hook off, and the dialogs stall the run again. A run whose permission mode is `plan` still ends in the
  `ExitPlanMode` approval dialog; only `EnterPlanMode` is denied.
- **A run's file tools reach its whole space folder** (ADR-0068). Every checkout and `work/` is readable and
  writable. Other instances' transcripts (`projects/*.jsonl`), settings and memory are readable but not writable.
  Everything in an instance's config dir that Claude Code loads, runs or restores at a later launch is denied to edits,
  for every instance, the run's own included, whatever their number: settings, the cached `remote-settings.json` and
  `policy-limits.json`, instructions (`CLAUDE.md`, `rules/`, agents, commands, skills, …), plugins, hooks, shell
  snapshots, auto memory and transcripts (`projects/`). The run's own auto memory lives outside the space folder
  (`<data_dir>/automemory/<space>/<instance>`); the memory an instance kept in its config dir before is copied there
  at its first launch on this release. The Claude Code and Codex login files are denied entirely. Three gaps remain:
  - in the folder of an instance created after the run started, and of every other instance in a space with more than
    50 of them, the files Claude Code doesn't load at launch (plan files, background job output) are writable;
  - the protected files and the stripped settings keys are deny-lists checked against Claude Code 2.1.317, so an entry
    a later version adds is not covered until SpaceMan adds it;
  - the node strips dangerous settings keys at every launch, but keeps keys the profile set.

  This is wider than before: the shell runs as the same OS user, but each shell command passes auto mode's classifier,
  which file tools inside a working directory skip. The folder of another space stays outside, and reading it raises a
  dialog, which the hook denies, unless a profile adds the node's whole spaces folder as a directory: its runs can
  then read and write other spaces' checkouts too. Every space's instance configuration, instructions, memory and
  transcripts stay denied to edits even then (the rules cover `<data_dir>/spaces/*`). A profile's own
  `autoMemoryDirectory` is ignored (the node logs a warning), so instances never share an auto memory folder.
- **The PermissionRequest hook can still end at the dialog** (ADR-0068):
  - **Timeout.** A hook that runs past its 30 s timeout ends at the dialog. A missing or failing agent binary does not:
    the command then prints a fixed deny.
  - **A profile hook that allows.** When a profile adds its own `PermissionRequest` hook, outside auto mode the first
    answer wins, so a profile hook that allows can win over SpaceMan's deny.
- **A run waiting at a terminal prompt raises no attention item.** SpaceMan doesn't detect that a run's terminal has
  been waiting for input for a long time. Follow-up: raise an attention item when the program status reports a
  waiting prompt for more than a few minutes.
- **Approvals are advisory unless a policy gates the operation.** Enforcement covers only operations SpaceMan performs:
  built-in agent tools and gateway tools. Shell commands and a harness's own permission prompts are not intercepted.
- **Agents call again after approval.** A gated call returns at once with a request; the agent must repeat the call
  with the same arguments after the notice. An approval belongs to the run that asked, so a relaunched run asks again.
- **Interrupted executions are not retried.** If central stops while an approved operation runs, its outcome is
  unknown; it is marked interrupted and needs a new approval to run again.
- **Ended requesters are not relaunched.** A response for a run that ended is routed to a live successor run, control
  or the operators, or held for the next run of the same instance; SpaceMan never relaunches the ended run to deliver
  it (ADR-0032, SM-HUM-009).
- **Response items carry no response.** With a policy's `response_inbox` (`routed` or `always`, ADR-0049) a response
  also becomes a `human_response` inbox item, but the item holds only metadata and a pointer to the action: the
  response stays readable only by the runs it concerns. Only answers make items (not withdrawals, expiries or
  clarification requests). The default is `off` (SM-INBOX-001).
- **Search is a substring match.** The search matches every word of the query as a substring of the title or the
  Markdown text, case-insensitively, over at most the space's 5,000 pending and its most recent final actions (the
  global list: the 5,000 oldest pending); there is no full-text index, ranking, stemming or search of comments and
  responses. Needs you lists pending actions only, so its status filter offers open and in progress (SM-HUM-006,
  SM-HUI-001).
- **Clarification answers come as comments.** An agent requester answers a clarification request with
  `spaceman_human_reply`; the request itself does not change (a corrected request is a follow-up). An operator
  requester sees the question on the action but is not notified unless their notification preferences say so and the
  UI is open (SM-HUM-006).
- **Notifications are in the web UI only.** Notification preferences decide what the open web UI announces to a user
  (in-app, and desktop notifications where the browser allows them). Nothing is announced while no SpaceMan page is
  open, and SpaceMan sends no e-mail or chat messages; external systems can follow the outbound `human.action.*` events
  (SM-HUI-006). A Telegram bot and Mini App for linked users exist (ADR-0083, SM-TG-001…014); see [Telegram](#telegram).
- **Retention deletes whole actions, daily.** A finished action is deleted with its comments and notifications once
  its space's `retention_human_actions_days` has passed, by one daily pass; there is no archive tier and no per-type
  period. Open actions and finished ones that open work still refers to are kept (ADR-0045, SM-HUI-007).
- **Exports are plaintext and whole-space.** A space export holds every human action with its response; there is no
  per-type, per-period or per-requester export, and the archive is not encrypted (ADR-0045).
- **Type permissions are role minimums per space.** A type's permission names the weakest built-in role that sees,
  asks with and responds to it, and which agent roles may ask; there are no per-user lists. Hidden actions are left
  out of the lists and the pending counts, but the attention summary still counts them per space and the change feed
  still announces their IDs (they read as not found). Agents' own view of actions they asked is unaffected
  (SM-HUI-008, ADR-0050).

## Control and repositories
- **Repository reports follow events, not a clock.** The node reports a space's configured repositories (remote,
  default branch, last fetch, changes, unpushed commits, worktrees) when a repository is added or edited, when the node
  connects, when a run of the space ends and on **Sync now**; nothing polls idle spaces (SM-ARCH-004). A report can be
  minutes old and says when it was made; work an agent is doing right now shows after its run ends or the next sync
  (SM-REPO-003). Each report asks every remote for its default branch (`git ls-remote`, 30 s each), so an unreachable
  remote slows the report down. Worktrees are never cleaned up automatically; remove them with git when done.
- **Reassignment does not move work.** Reassigning a task (the task drawer, `spaceman task update --role/--run`, the API)
  records the new role and run; it never stops the run working on it or starts another: control acts on the new
  assignee. A run of another space is refused; a run ID that matches no run is accepted as given (SM-CTRL-004).
- **Incidents only from trusted triggers; pausing is unexercised with real harnesses.** A space's queue policy names
  who may raise incidents (operators and control by default; event subscriptions and templates when listed). Workers,
  schedules, other spaces and the other inbox sources can never be trusted, even when control relays them: their
  incidents become proposals an operator confirms, or are refused (SM-QUEUE-009, ADR-0050). Trusting control trusts
  its judgement, which reads untrusted content; remove `control` to make it relay trusted sources only. A preempted
  task's workers finish, are paused (their turn interrupted) or are stopped, as the policy says. Pausing sends
  herdr-core's interrupt (Esc or Ctrl+C) and has been verified only with the in-memory runner; a paused worker stays
  paused until it is resumed, and automatic resumption only restarts tasks that still have a live worker (others wait
  for control to launch one).
- **Repository credentials.** Configured repositories are cloned and fetched with the node's own Git authentication
  (SSH keys, credential helpers, Hallpass Git roles), unless a repository has an SSH key SpaceMan holds (ADR-0057).
  SpaceMan refuses URLs that carry credentials, so a remote that needs a token in its URL cannot be configured
  (SM-REPO-005, ADR-0044). A SpaceMan SSH key:
  - is used only over SSH;
  - is used only for the node's own clones, fetches and `ls-remote` of that repository. Agents' git commands in their
    terminals don't get it;
  - needs node protocol 1.7 (agent 0.1.0-rc.6 or later). An older agent ignores it, and the repository shows a note.

  While a clone or fetch runs, the key is offered on a private agent socket. Agents run as the same Unix user as the
  node agent, so one could connect to that socket and have it sign. It can never read the key, and with a read-only
  deploy key that gives no more than the checkout the agent already has.

  Shared Git SSH keys (ADR-0058) behave the same way, but open every repository their Git account can read: use a
  read-only machine user. Built-in host-key pins cover github.com only; other hosts need the repository's known_hosts. A relocation's
  destination does not clone repositories that have a key: the first sync after the move does. Production accepts `https://`, `ssh://` and scp-like URLs only; `http://`, `git://`,
  `file://` and local paths need development mode.
- **Some permanent delivery failures are retried.** A node command central can't prepare for a transient reason is
  retried with a backoff (250 ms up to 30 s) while the node is connected. A run's command whose stored payload doesn't
  decode fails the run as `invalid_command`, but a few failures that can't heal are retried the same way, with a
  warning in central's log on every pass: a repository SSH key that can't be decrypted, and a command not tied to a run
  (a repository sync, a relocation's prepare) whose payload doesn't decode. Fix the key or remove the repository.
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
- **The control watchdog reminds; it does not make control act** (ADR-0099). A reminder is a line typed into control's
  terminal, naming counts only; whether control acts on it is up to the agent. The watchdog's state is in central's
  memory and starts over when central restarts (which can only delay a reminder). It judges backlog from SpaceMan's own
  state: work it cannot see (a pull request waiting for a person, a repository change) is not backlog. Restarting
  control on escalation is off by default and stays within the supervisor's restart budget. It is on by default for
  nudging: set `control_watchdog_minutes` to 0 to turn it off. Simulated evidence only (fake clock, scripted runs).
- **Runs an agent restart stopped are resumed a limited number of times** (ADR-0098). At most 3 automatic resumes per
  chain (a resumed run that lived 30 minutes starts again); then the task goes back to `ready` and control is told. A
  run without a task, a task already in review, a run somebody asked to stop, a control run, and a run whose node was silent so long that the
  stale-run sweeper ended it are not resumed. A run that finished no turn starts again with its original prompt rather
  than resuming a session. Simulated evidence only (scripted node, fake clock): not verified with a real agent restart.

## Recordings
- **A full disk on the node leaves a gap in the node's local recording** (ADR-0097). The run is unaffected, and its
  output still reaches central's copy and the live view. The node tries the disk again with a backoff (5 s, up to a
  minute) and the local recording resumes in a new segment once space is freed; output while it was full is missing
  from the local copy only, and also from central's if it could not be shipped meanwhile (the node disconnected for
  more than about 1 MiB of output, or the agent restarted while disconnected). Recordings of nodes older than this release end with a `trunc` record (`disk`)
  at the failure instead.
- **A write failure on central's own disk** stops central's copy of that recording, and nothing marks it there. The
  node keeps its local recording. Tested: node-side only (`make test-fault`).
- **Retention deletes central's copy only.** `retention_recordings_days` deletes central's copy of an ended run's
  recording; the node's local recordings are never pruned by SpaceMan (remove old `runs/<id>/recording` folders on the
  node yourself). The recording size limit (`recording_quota_bytes`) reaches nodes at launch: an agent older than this
  release ignores it and keeps 512 MiB, and a run keeps the size it started with (SM-REC-003).
- **Old recording directories with no journal row are still kept** (ADR-0096, design only). Since the acknowledgement
  fix, central acknowledges only records it synced to disk, so a node no longer deletes the only copy of a record
  central failed to store. Directories that an earlier release left behind with segments and no journal row remain on
  the node: the node cannot tell them from a run whose journal was lost, and reclaiming them waits for central to
  confirm what it holds (protocol minor 15, not implemented).
- **A recording that failed on central is not reopened for 5 seconds.** Chunks in that window are refused and the node
  re-ships them afterwards; a persistent storage failure keeps the connection closing and the run's recording
  incomplete on central until the disk is fixed.

## Web UI
- **The attention summary covers central, not node disks.** **Attention** (and each space overview's counters) gathers
  pending human actions, blocked and failed tasks, quarantined events, unknown and recently failed runs, budget notices,
  attachment storage near its quota, disconnected nodes, outbound dead letters, failing backups and central's storage
  health. Node disks are not reported: a full node disk shows only as a warning in the node agent's log (and, on nodes
  older than this release, as a truncated recording labelled "Truncated (quota)"). Storage health has no change-feed event, so that section refreshes with other changes
  or on **Refresh**. Memory sync state and knowledge-base work stay on their pages (SM-UI-003).
- **Terminal takeover is time-bound and keeps the terminal's size.** An admin (`terminal:takeover`) can take over a
  running terminal; automated input waits until it ends. It ends 30 seconds after the holder's page stops renewing it,
  so revoking the holder's session ends it within 30 seconds (they cannot type after revocation). Taking over does not
  change the terminal's size (see the next item). Agents older than node protocol 1.6 cannot be taken over, and the
  typed-byte count in the audit record is per central process (ADR-0046, SM-UI-004).
- **A terminal has one size, shared by everyone watching.** A program runs at the node's terminal size
  (`runner.terminal_size`, default 160×48), at its space's `terminal_cols` × `terminal_rows` when both are set, or at
  the size an admin set from the web (fit to their window, a preset or a custom size, ADR-0075). The size is per run,
  not per viewer: the view scales the font to the width, follows the cursor and scrolls with the wheel, and opening a
  page never resizes the program. Resizing needs node protocol 1.10; the node holds a set size through one extra
  herdr-core connection per sized run. The Terraform provider's `spaceman_space_limits` resource has no attributes for
  the two limits yet (the API, `spaceman limits set` and `spaceman run terminal-size` work).
- **Scrolling back reaches only what the terminal keeps.** The live view scrolls back through the browser terminal's
  scrollback (a 24 MiB limit: at least 10,000 rows at 160 columns, ADR-0080; a page reload rebuilds it from the stored
  recording), never by sending the run input. A terminal first loads only the last 1 MiB of output; earlier output loads
  when the viewer asks at the top of the scrollback (ADR-0081). A tail restores the terminal size and the DEC private
  modes in effect where it starts, not colours, and a full-screen program's rows that it never repaints can be missing
  from a tail until earlier output is loaded. A full-screen program on the alternate screen keeps its history itself,
  so watchers see only its current screen: Claude Code runs therefore use its classic renderer unless the profile sets
  `"tui": "fullscreen"`, and other full-screen
  programs (Codex's alternate screen, editors) are the same. During a takeover the wheel scrolls such a program only
  when it asks for mouse input (or xterm's alternate-scroll mode) (ADR-0064).
- **Live views and many tabs over plain HTTP.** A browser opens at most six HTTP/1.1 connections to a host. The change
  feed is shared by all tabs where Web Locks exist (HTTPS, or `localhost`); a hidden tab releases its live terminal after
  5 seconds. Over plain HTTP to another host each tab holds its own feed, so only a few tabs work at once: serve central
  over HTTPS (HTTP/2) for more (ADR-0052).
- **TanStack Table on some lists.** The attention summary and the nodes page use the shared TanStack Table component
  (sortable headers, a filter box); other lists are Mantine tables or card lists. TanStack Store holds per-browser view
  preferences (table sort and filter, human-action filters); server data stays in TanStack Query (SM-UI-001).
- **View controls keep React state.** Every form uses TanStack Form, including the schema-driven human-action fields.
  Filters, tabs, version pickers and open or closed flags are view state, not forms, and stay React state or TanStack
  Store (SM-ATT-007).

## Attachments
- **Attachments are added when writing, not later.** Inbox items, tasks (at creation), human-action requests, responses
  and comments, and messages and replies reference attachments of their space. Editing a task does not add
  attachments, and agents reference attachments only as `spaceman://attachment/<id>` links in their text (they cannot
  upload) (SM-ATT-001, SM-ATT-002).
- **Attachment retention follows references, not Markdown.** An attachment expires once every item that references
  it (an inbox item's attachment list) is finished. A `spaceman://attachment/…` link pasted into another item's text is
  not a reference: that link breaks when the attachment expires (SM-ATT-006).
- **Exports exclude recordings and transcripts**, and an export is not encrypted (ADR-0045). Download recordings and
  transcripts per run.
- **Agents fetch attachments through `spaceman_attachment_get`** (text, or base64 up to 512 KiB per call, with ranged
  reads; a larger binary is not returned inline) **or `spaceman_attachment_save`**, which copies the whole file into the
  run's own folder on the node until the run ends (SM-ATT-005, ADR-0073). Attachments are not copied there unasked.
  There is no server-side image downscale: the harness's own file reader scales images. A saved file stays until its
  run ends; files of a run that ended while its node agent was not running are removed when the node reconciles at
  its next start, or at the instance's next launch. A node that never starts again leaves them on disk.

## Audit and logs
- **Audit retention trusts its own checkpoint.** After `retention.audit_days` pruned events, verification starts from
  the recorded hash of the last pruned event. Someone able to rewrite the database could also rewrite that checkpoint;
  this is the same boundary as the whole chain (THREAT_MODEL). There is no export of the pruned events: copy the log
  forward (`GET /audit?after=`) before they are pruned (SM-OBS-003).
- **The audit page has no forward view.** The UI pages newest first; the forward cursor is in the API and CLI.
- **No HTTP access log.** Central logs errors and component events, not every request; request counts and latencies
  are metrics (SM-OBS-001).

## Messaging
- **A forced "Message the agent" can still be dropped.** The run page's box types the text at once (`force`), but a
  program reported as `blocked` (a permission prompt, for example) refuses it, as does a takeover that starts in
  between or a run that is ending. Central has already answered 202, so the operator learns of it afterwards: the run
  page shows a "Message not delivered" toast (not for changes older than a minute, such as a replay after a
  reconnect) and the audit log records a `run.input_rejected` event with the node's reason. The text is not retried;
  send it again. Operator input containing control characters (C0, DEL or C1, except tab and newline) is refused up
  front with a 400, so an escape sequence cannot act on the program (ADR-0084).
- **Terminal notices need an idle program.** Notices are typed only when the harness reports idle or done through
  OSC 7501; a program that never reports status gets no notices and must call `spaceman_messages` itself. herdr-core's
  own gate accepts only idle, so a `done` program gets the notice ungated after a fresh status check (ADR-0052).
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
- **Changing a user's email is not an SSO change.** Identities are matched by issuer and subject, never by email, so
  editing a user's email never relinks an identity, and SpaceMan does not copy a provider's later email changes to the
  user. Changing a password user's email ends their sessions (it is their sign-in name); API tokens keep working
  (SM-API-004, ADR-0050).

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
- **Backups copy the S3 artifact store whole.** With `artifacts.backend: s3` every artifact in the bucket is
  downloaded into each backup (ADR-0048), which takes time and backup space for a large bucket. A restore writes
  the artifacts to the local folders; run `spaceman artifacts migrate --to s3` before starting central.

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
  configuration, and node commands use fixed timeouts. See the configuration reference
  (`site/reference/configuration.md`).
- **The S3 artifact store** (`artifacts.backend: s3`, ADR-0048) is verified against MinIO only (`TestRealS3Artifacts`
  in `make test-real-s3`); AWS S3 and Cloudflare R2, virtual-hosted addressing and listings past 1000 keys are
  unverified. Objects are not encrypted by SpaceMan (as on the local disk): use a private bucket, with server-side
  encryption if wanted. One prefix serves one installation (garbage collection deletes unreferenced attachment
  content under it). Each object is a single PUT (at most 5 GiB). Central still needs local disk for the spool:
  recordings being shipped, transcripts being received and uploads in progress. Recordings are archived to the bucket
  only once their run ends; a recording or transcript whose upload failed stays on the local disk (and is served from
  there) until central's next start. Switching the backend moves nothing: stop central and run
  `spaceman artifacts migrate`. Recordings and transcripts of runs that are not in a restored database stay in the
  bucket.
- **Upgrades are unexercised.** No upgrade from one release to another has run. Central and nodes on different versions
  are untested.

## Terraform provider
- **Phases 1 and 2, not published.** The provider (its own repository, `terraform-provider-spaceman`, ADR-0047) covers what the API supports today (ADR-0035,
  ADR-0041), space repositories (ADR-0044), human-action type permissions and user profile edits (ADR-0050). The backup
  target and schedule (configuration file only) are not managed (SM-API-004). Terraform 1.11 or later is required
  (write-only arguments).
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
- **Optional template filters are not in the provider yet.** Its definition type has no `if_set` or `unless_set` and
  refuses unknown fields, so a custom template or subscription definition using them cannot be managed through
  Terraform, and the `spaceman_event_template` data source drops them when it reads a built-in that has them
  (ADR-0061). Subscribing to the built-ins by slug works.
- **Write-only rotation is explicit.** Terraform cannot see a changed write-only secret: bump its `_wo_version`.
- **Tested against a local central only** (simulated, `make acc` in the provider repository); no Terraform run against a production
  deployment, a real identity provider or a real Pulse deployment is recorded.

## Extension points
- **One implementation per interface, mostly.** Every adapter kind has a Go interface (SM-ARCH-003, ADR-0048):
  harnesses (`harness.Harness`), runners (`runner.Runner`), artifact storage (`artifacts.Store`: local and S3),
  outbound publication (`outbox.Publisher`), inbound event sources (`events.Source`: Pulse durable consumers and core
  NATS) and MCP upstream transports (`gateway.Transport`). Adapters are compiled in. The gateway speaks only streamable
  HTTP to upstream servers (no stdio or SSE-only transport), and Pulse is the only inbound event system.

- **Central has no per-IP or global connection cap** (T-43, ADR-0085). Slow-body clients are bounded per connection (5 s on
  a JSON operation and on the enrolment and MCP endpoints, 10 minutes on an attachment upload, which needs a valid
  credential, 30 s without progress), but many connections
  at once each hold a goroutine for that long. A reverse proxy or firewall in front of central is the place for connection
  limits. A client below about 43 KiB/s can't upload a 25 MiB attachment within its 10 minutes.

## Telegram

Status: built and tested against a fake Bot API server only; **never run against a real bot** (SM-TG-001…014 stay
"tested (simulated)" in [TRACEABILITY.md](TRACEABILITY.md)). The user guide is the site's Telegram page.

- **Web only.** These are refused from Telegram by classification (not hidden in the UI), in the bot and the Mini App
  alike: everything that needs a step-up, re-authentication or MFA, and every personal session operation (API tokens,
  passwords, sessions, identities, recovery codes); linking itself is started and confirmed in the web; terminal takeover,
  typing into a terminal, terminal sizes and run launches; secrets, accounts, credentials, Git keys, Harbor Pilot, MCP
  servers, skills, prompts, roles, policies, human type definitions and permissions, event connections, templates and
  subscriptions, cross-space settings; users, grants, SSO providers, the root token, audit verification, backups,
  restore, purge, retention, deleting recordings and transcripts, nodes, relocation and space state; all Telegram
  configuration; bulk approvals; HallPass approvals; the MCP endpoint; and anything in a space whose Telegram content
  level is `off`. Recording downloads, transcripts and space exports are never served to Telegram. Task creation is in the
  Mini App only (not in chat).
- **Enforced approvals are decided in the browser** unless the space sets `enforced_approvals = confirm`. Withdrawing an
  approval is never possible from Telegram, and a group never decides one.
- **The terminal is watch-only and opt-in.** A run's page in the Mini App shows its live terminal only when the space
  turns `terminal_watch` on and the link holds `terminal:watch`; never with input, takeover, resizing or replay. Turning
  the setting off, or hiding or disabling the space, ends open streams at once. It exists only in the Mini App, never in a chat.
- **Chat content lives on Telegram's servers**, not end-to-end encrypted, within the space's content level. Terminal
  output, hidden responses, non-allowlisted attachment contents and secrets are never sent to a chat. Forwarding and
  saving are turned off for messages with a body, which doesn't stop screenshots.
- **Private chats only, unless a forum group is bound.** The bot leaves at once a chat that can never be bound (a channel,
  a basic group, a supergroup without topics), waits 15 minutes in an unbound forum group and then leaves, and leaves a
  group whose binding is lost. The wait is in memory: after a restart the first update from an unbound group makes the bot
  leave, and a group that stays silent stays until it produces one. Binding needs the admin-only `telegram_group:write`.
  Telegram has **no `web_app` buttons in groups**, so a group's forms open the Mini App through a direct link instead,
  which needs the bot's Main Mini App to be set in BotFather.
- **The content level is summary-only in groups.** Whatever the space's content level, a group gets summaries, and no
  attachment is ever sent to a group chat. Group membership grants nothing: each tap is authorized as the tapping
  person's own link. A group answers only `/inbox`, `/needsyou`, `/queue`, `/runs` and `/messages` (and a short refusal to
  `/spaces`, `/space` and `/control`); every other command, and a command for another bot, is ignored silently, so `/needs`
  works in a private chat only.
- **One bot per central**, one live link per Telegram account, and a different bot revokes every link.
- **No Telegram sign-in.** A link never signs anyone in to the web app.
- **In-memory state is lost on restart:** Mini App sessions and replay memory (see the next section), conversation steps in chat,
  the collected digest, pending confirmations and the update queue. A webhook update that was acknowledged but still queued
  when central stopped is lost; the bot acknowledges captures so people can resend.
- **Digest and quiet hours need a time zone,** per user. Without one both are off; there is no server-wide zone.
- **Telegram's own limits apply:** messages are rate-limited per chat and group, long content is split or cut, and the
  sender treats a chat that answers 403 as blocked (its queue is dropped, in memory only; the link is not marked).
- **Screenshots of the Mini App are not part of the guide** (the capture pipeline covers the web app only).

## Telegram Mini App

- **Download links live in memory.** A signed download link (ADR-0083 §3.4.3) is held by central for 60 seconds and 3
  requests, at most 16 per Telegram link (a new one replaces that link's oldest) and 10,000 in all (past that a mint is refused with 429, never by evicting another link's grant); a restart drops them all and the Mini App mints a new one. A
  refused request (revoked link, removed grant) still counts as a use, and every refusal is the same 404.
- **No `tg.html` until the client shell ships.** Central serves the Mini App page from the build when `tg.html` is in
  it (TG-9b adds the entry); without it the Mini App host answers 404 for pages.
- **Sessions live in memory.** Mini App sessions (ADR-0083 §3.4.2) are held by central only: a restart, a bot change
  or turning the Mini App off ends them all, and the user reopens the app. Unsaved form input is lost.
- **Session capacity can run out.** Central holds at most 20,000 Mini App sessions (5 per link, the oldest of a link
  dropped). When every slot holds a live session, new exchanges are refused with 429 until sessions expire (idle 15
  min, absolute 1 h by default). Only valid initData of linked users opens a session, so only linked Telegram accounts
  can use the capacity up, at 10 exchanges a minute each (SM-TG-005).
- **Replay memory is bounded.** Used initData is remembered for an hour (the largest allowed max age), at most 10,000
  entries; under a flood of distinct valid initData the oldest are forgotten first (SM-TG-005). Forgetting one that has
  not expired raises an auth_date watermark: initData issued at or before it is refused, so a flush can't revive a used
  one, and a user whose initData is that old reopens the app for fresh data.
- **Refused-exchange audit is aggregated.** Refusals are counted per address (an IPv6 /64 is one address), at most one
  audit row per address a minute and 60 rows a minute in all. Refusals the cap withholds, and those an evicted counter
  still held, are added to one global count that is reported as a single overflow row a minute (source address `*`, outside
  the 60-row cap), so the rows add up to every refusal; the overflow row appears with the next refusal after its minute,
  so the latest count can wait for it. At most 10,000 addresses are tracked, the oldest dropped first, so a flood from very
  many addresses costs per-address detail (an address's refusals then show only in the overflow row), not counts.
  An address's held-back count waits for its next refusal or its eviction before it is recorded.
- **Unlinked Telegram users can fill the replay cache.** Valid initData from any Telegram user is remembered before the
  link check, so users without a link can push entries through the 10,000-entry cache like linked ones (the watermark above
  still keeps a flushed initData from being reused).
- **A token rotation ends Mini App sessions.** Each session is bound to a hash of the bot's signing key; a new bot token
  (same bot ID) refuses the sessions opened under the old one.

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). Verified end to end on macOS with hc 0.1.16 (`test/real` `TestRealNodeEndToEnd`:
  a `SIGKILL`ed agent's run is ended by the guard; rerun 2026-11-03); not yet verified on Linux, because no herdr-core is
  installed in the Linux container the systemd suite uses (ADR-0034).
- **External integrations still without real evidence** (SM-VER-002, re-checked 2026-11-03):
  - a real Codex run: no Codex or OpenAI credential on the development machine;
  - a real Claude usage limit on a SpaceMan run (ADR-0079, K-16): none observed; the usage-limit tests are simulated;
  - AWS S3 and Cloudflare R2: no account (MinIO only);
  - a physical or cloud Linux host, and linux/amd64 under systemd (Docker Desktop's amd64 emulation breaks systemd);
  - Google behind Porch and the GitHub API behind go-pulse-github: no accounts (Porch's fake Google fixture and a local
    GitHub App stand-in serving pulse-github's own test JSON were used);
  - TLS and production profiles of Porch and pulsed: both ran in development mode on loopback.
- **Terminal byte stream has no sequence numbers** (herdr-core 0.1.16). After the node reattaches, SpaceMan records an explicit gap and the new screen repaint. It cannot recover output that was emitted while it was detached.
