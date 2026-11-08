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
- **Older gateways are detected by a feature probe.** Harbor Pilot has no version endpoint; a gateway that does not know
  `check_budget` on mint (before 0.1.3) is refused. A later gateway that dropped the field would look old too.
- **Demo evidence only.** Everything real ran against `harbor-pilot serve --demo` 0.1.3 with its synthetic upstreams:
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
- **A run's file tools reach its whole space folder** (ADR-0068): other instances' transcripts, settings and checkouts
  too, not only its own. Only the Claude Code and Codex login files there are denied. The folder of another space
  stays outside, and reading it still raises a dialog, which the hook denies.
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
  (SM-HUI-006).
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
- **The attention summary covers central, not node disks.** **Attention** (and each space overview's counters) gathers
  pending human actions, blocked and failed tasks, quarantined events, unknown and recently failed runs, budget notices,
  attachment storage near its quota, disconnected nodes, outbound dead letters, failing backups and central's storage
  health. Node disks are not reported: a full node disk shows only as a truncated recording on its run, labelled
  "Truncated (quota)" even then. Storage health has no change-feed event, so that section refreshes with other changes
  or on **Refresh**. Memory sync state and knowledge-base work stay on their pages (SM-UI-003).
- **Terminal takeover is time-bound and keeps the terminal's size.** An admin (`terminal:takeover`) can take over a
  running terminal; automated input waits until it ends. It ends 30 seconds after the holder's page stops renewing it,
  so revoking the holder's session ends it within 30 seconds (they cannot type after revocation). The terminal keeps its
  size (the node's `runner.terminal_size`, 160×48 by default) rather than fitting the browser. Agents older than node protocol 1.6 cannot be taken over, and the
  typed-byte count in the audit record is per central process (ADR-0046, SM-UI-004).
- **Terminals do not fit the browser.** A program runs at the node's terminal size (`runner.terminal_size`, default
  160×48; runs started before that setting keep herdr-core's 200×120). The view scales the font to the width, follows
  the cursor as output arrives, and scrolls with the wheel; a watch-only view never resizes the program (ADR-0052).
- **Scrolling back reaches only what the terminal keeps.** The live view scrolls back through the browser terminal's
  scrollback (10,000 lines; a page reload rebuilds it from the stored recording), never by sending the run input. A
  full-screen program on the alternate screen keeps its history itself, so watchers see only its current screen: Claude
  Code runs therefore use its classic renderer unless the profile sets `"tui": "fullscreen"`, and other full-screen
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
