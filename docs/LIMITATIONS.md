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

## Pending verification (not yet guaranteed)
- **Dead-daemon enforcement** (ADR-0017) uses a lease-guard parent terminal. hc's parent/child semantics were verified (spike S-2). The
  mechanism needs the hc core and holder to be alive, and it can be defeated by a node operator. It cannot prove that processes an agent spawned
  outside hc have stopped (SM-NODE-008). The end-to-end SpaceMan test is still pending (M1-10).
- **Terminal recording** depends on the herdr-core framed attach protocol (K-1).
