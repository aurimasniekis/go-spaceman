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

## Human actions
- **Approvals are advisory.** An approval decision is recorded, audited and sent to the requester, but SpaceMan does not
  yet enforce it on the operation; digest-bound enforcement arrives with the approval gateway (SM-APPR, M3).
- **Agents cannot create human actions yet.** Operators create them in the UI or API; the agents' MCP tools arrive with
  control and workers (SM-HUM-008, M2-06). Notifications for agent requesters are stored durably, but no delivery
  adapter consumes them yet (SM-HUM-009).
- **Default types only.** Custom types with schema-driven forms arrive in M3 (SM-HTYPE). Reminders and escalation
  arrive in M4; expiry runs once a minute.

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). Verified end to end on macOS with hc 0.1.16 (`test/real` `TestRealNodeEndToEnd`:
  a `SIGKILL`ed agent's run is ended by the guard); not yet verified on Linux.
- **Terminal byte stream has no sequence numbers** (herdr-core 0.1.16). After the node reattaches, SpaceMan records an explicit gap and the new screen repaint. It cannot recover output that was emitted while it was detached.
