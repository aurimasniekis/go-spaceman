# Architecture

SpaceMan has two programs and depends on a few external systems. This page shows how they fit together and where each
kind of state lives. [Concepts](./concepts) explains the vocabulary (spaces, runs, leases), and the
[security model](/security/) explains which boundaries the design protects and which it does not.

## Components

```mermaid
flowchart LR
  subgraph People["People and automation"]
    B["Browser (embedded web UI)"]
    K["spaceman CLI, Terraform, scripts"]
  end
  subgraph Central["Central: spaceman serve"]
    API["HTTP API (OpenAPI 3.1) and change feed"]
    SVC["Services: spaces, queue, inbox, human actions, approvals, KB, memory, events, outbox, schedules, backups"]
    GW["MCP gateway and agent tools"]
    DB[("SQLite (WAL) in data_dir")]
    ART[("Artifacts: recordings, attachments, transcripts (local directory)")]
  end
  subgraph Node["Node: spaceman-agent"]
    AG["Agent daemon and journal"]
    HC["Its own herdr-core core"]
    LG["Lease-guard terminal"]
    R["Claude Code / Codex runs"]
    BR["MCP bridge (per run)"]
  end
  PULSE[("Pulse (NATS JetStream)")]
  UP["External MCP servers"]
  IDP["Porch or another OIDC provider"]
  B -->|"HTTPS, session + CSRF"| API
  K -->|"HTTPS, bearer token"| API
  API --> SVC
  SVC --> DB
  SVC --> ART
  AG -->|"outbound WebSocket, node credential"| API
  AG --> HC
  HC --> LG
  LG --> R
  R --> BR
  BR -->|"tool calls with the run credential"| GW
  GW -->|"per-run sessions"| UP
  PULSE -->|"durable or core subscriptions"| SVC
  SVC -->|"outbox relay"| PULSE
  B -.->|"sign-in redirect"| IDP
```

- **Central** is one Go binary with the web UI embedded. All state is in one SQLite database under `data_dir`, written
  through a single writer, plus an artifact store for attachments, recordings and transcripts: the local disk or an
  S3-compatible bucket ([Artifact storage](/operators/storage)). There is exactly one active central; a process lock on the data directory prevents a
  second one.
- **Nodes** run `spaceman-agent`. The agent always dials central; central never connects to a node. It runs every
  harness inside its own [herdr-core](https://github.com/herdrdev/herdr-core) core (never the operator's default
  core), records each terminal, and keeps a local journal so a restart adopts what is running instead of starting it
  again.
- **Agents never talk to central's database or to other agents directly.** Their SpaceMan tools and every gateway
  call go through the run's MCP bridge to central, which authorizes each call (Casbin), applies limits and approvals,
  and audits it.

## Where state lives

| State | Kept by | Notes |
|---|---|---|
| Users, grants, spaces, runs, queue, inbox, human actions, approvals, messages, subscriptions, audit log | Central's SQLite | Hash-chained audit; optimistic concurrency (`If-Match`) and idempotent creates on the API |
| Secrets (harness accounts, Pulse credentials, MCP headers, OIDC client secrets) | Central's SQLite, encrypted | Data keys wrapped by the KEK, which lives outside `data_dir` ([backups](/operators/backup#key-recovery)) |
| Recordings, attachments, transcripts | Artifact store | Recordings are written on the node and shipped to central |
| Space folders, Git repositories, worktrees | The space's node | Not in central; [relocation](/operators/relocation) moves them deliberately |
| Role memory | The node (working copies), central (acknowledged Git history) | Central guarantees only acknowledged history ([role memory](./memory)) |
| Knowledge bases | Git repositories in the space | Only node-side acceptance moves the knowledge branch ([knowledge bases](./knowledge)) |

## A run, end to end

```mermaid
sequenceDiagram
  participant O as Operator or control
  participant C as Central
  participant A as spaceman-agent
  participant H as herdr-core
  participant R as Harness run
  O->>C: launch (space, role, account)
  C->>C: resolve prompt, profile, skills and limits into a manifest
  C->>A: launch command (lease generation, idempotency key)
  A->>H: find the run's terminal by label, else start it under the lease guard
  H->>R: start with a private config dir and a one-run credential
  A-->>C: run report (running) and recording segments
  R->>A: SpaceMan tool call (MCP bridge)
  A->>C: tool call with the run credential
  C-->>R: result (authorized, limited, audited)
  R-->>A: exits
  A-->>C: final report, transcript and usage
```

## Event ingestion

Pulse delivers events (for example from go-pulse-github) to each subscribed space. Durable subscriptions use one
JetStream consumer per subscription, so two spaces subscribed to the same subject both receive it, and a restart
resumes from the cursor. [Events and subscriptions](./events) covers templates, quarantine and replay.

```mermaid
flowchart LR
  P[("Pulse: JetStream stream")] -->|"one durable consumer per subscription"| S["Subscription (space, template)"]
  S --> V{"Valid and matching?"}
  V -->|"no: invalid"| Q["Quarantine (reprocess or discard)"]
  V -->|"no: filter did not match"| D["Delivery recorded as filtered"]
  V -->|"yes"| X{"Seen this event before?"}
  X -->|"yes"| A["Delivery recorded as duplicate, no new item"]
  X -->|"no"| I["Inbox item (untrusted content)"]
  I --> CT["Control decides what to do"]
```

## Human actions and enforced approvals

A human action is advisory: an agent asks a person and continues when notified. An **enforced approval** gates an
operation SpaceMan itself performs, bound to the run and a digest of the exact arguments. [Human actions](./human-actions)
and [enforced approvals](./approvals) describe both.

```mermaid
stateDiagram-v2
  [*] --> open: agent or operator asks
  open --> in_progress: claimed
  in_progress --> open: unassigned
  open --> resolved: response (outcome recorded once)
  in_progress --> resolved: response
  open --> cancelled: withdrawn
  open --> expired: expiry passed
  resolved --> [*]
  cancelled --> [*]
  expired --> [*]
```

## Leases and failure

A node may run a space's work only while it holds that space's **execution lease**, renewed with every heartbeat. Each
lease has a generation, and anything from an older generation is fenced off. When central is unreachable, the node
starts nothing new and, after the lease and `nodes.disconnect_grace`, interrupts or stops the space's runs; when the
agent itself dies, the lease guard ends them. [Nodes and leases](/operators/nodes) has the details, and
[relocation](/operators/relocation) shows how a new generation fences the source node when a space moves.

## Further reading

- [Configuration reference](/reference/configuration) and the [HTTP API](/reference/api)
- [Deployment overview](/operators/overview) and [troubleshooting](/operators/troubleshooting)
- [Threat model](/security/threat-model) and [architectural decisions](/security/decisions)
