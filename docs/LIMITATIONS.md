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

## Accounts
- **No Codex ChatGPT login.** Codex runs use an OpenAI API key; ChatGPT-plan logins (`auth.json`) are not supported yet (K-9).
- **No real Codex run yet.** The Codex adapter is tested with fakes; only Claude Code has real-run evidence.
- **Claude first-run keys are observed, not documented.** SpaceMan seeds `.claude.json` keys seen in Claude Code
  2.1.316 (ADR-0021); a Claude Code upgrade must be re-checked with the opt-in real suite.

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
  reported; relocation and restore handling arrive with M4.
- **Flushes before relocation and backups** arrive with those features (M4).

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

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). Verified end to end on macOS with hc 0.1.16 (`test/real` `TestRealNodeEndToEnd`:
  a `SIGKILL`ed agent's run is ended by the guard); not yet verified on Linux.
- **Terminal byte stream has no sequence numbers** (herdr-core 0.1.16). After the node reattaches, SpaceMan records an explicit gap and the new screen repaint. It cannot recover output that was emitted while it was detached.
