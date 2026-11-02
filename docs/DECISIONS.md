# SpaceMan — Architectural Decisions

> ADR log and open items. Each ADR has **Status · Context · Decision · Alternatives · Consequences**.
> Statuses: `accepted`, `proposed`, `superseded by ADR-NNNN`, `withdrawn`. ADR numbers are never reused.
> A decision that weakens a GOAL.md security requirement is labeled **security-relevant change**.

## 1. Decision log

### ADR-0001 — Modular monolith, single active central instance, cold standby
- **Status:** accepted (2026-11-02)
- **Context:** SM-INV-001 requires one active control plane that owns SQLite. Hallpass solved the same problem the same way (its ADR-0001).
- **Decision:** One Go binary `spaceman` (`serve` plus admin and client subcommands). An exclusive `flock` on `<data>/spaceman.lock`
  enforces one process per data dir. Recovery is restore-from-backup on a new host. SQLite on NFS/SMB is refused.
  A second binary, `spaceman-agent`, runs on nodes.
- **Alternatives:** active-active with PostgreSQL (out of scope by brief); replicated SQLite (adds failure modes).
- **Consequences:** Simple deployment. When the central host is lost, nodes enter disconnect policy (SM-NODE-007) until central returns.

### ADR-0002 — Storage stack follows Hallpass: modernc SQLite, WAL, split pools, goose, sqlc
- **Status:** accepted
- **Context:** Needs CGO-free static builds and predictable concurrency. Both Hallpass and Pulse use `modernc.org/sqlite` v1.61.0.
- **Decision:** `modernc.org/sqlite`. One writer connection with `_txlock=immediate`. N `query_only` readers. Pragmas: WAL,
  `synchronous=FULL`, `foreign_keys=1`, `busy_timeout=5000`, `trusted_schema=0`. Forward-only `pressly/goose` migrations from an
  embedded FS, with a `VACUUM INTO` snapshot before migrating. `sqlc` generates queries. Timestamps are INTEGER Unix microseconds.
  IDs are `<prefix>_` + 26-char lowercase Crockford base32 of a UUIDv7.
  `TxNamed(ctx, name, fn)` retries `SQLITE_BUSY`, and `tx.AfterCommit` carries side effects. The node journal uses the same stack.
- **Consequences:** Static multi-arch binaries. Callbacks must be safe to re-run.

### ADR-0003 — API is code-first OpenAPI 3.1 with huma v2
- **Status:** accepted
- **Context:** Hallpass hand-writes `api/openapi.yaml` and generates a dispatcher. Pulse uses `danielgtaylor/huma/v2` (v2.39.1) on
  stdlib `net/http`, which generates OpenAPI 3.1 from Go types and emits RFC 9457 errors natively. SpaceMan's API surface is very large.
- **Decision:** huma v2 on stdlib `http.ServeMux`. The generated `api/openapi.json` is committed and drift-checked in CI. The web client
  uses `openapi-typescript` + `openapi-fetch`, as Pulse does. HTTP conventions (problem `code` values, `If-Match: "<version>"`,
  `Idempotency-Key`, opaque cursor pagination with `{items,next_cursor}`, integer `version` per resource) follow Hallpass.
- **Alternatives:** hand-written spec plus generator (Hallpass): more drift risk at this scale.
- **Consequences:** One Go type is the single source for the server, docs, the TS client and the Terraform client.

### ADR-0004 — Authorization with Casbin (RBAC with domains)
- **Status:** accepted (operator direction, 2026-11-02)
- **Context:** The brief allows Casbin, and the operator asked for it explicitly instead of a hand-written matrix (Hallpass `internal/rbac`).
  SM-AUTH-003/004 need viewer/operator/admin roles with space scopes and separate permission objects.
- **Decision:** `github.com/casbin/casbin/v2` with an embedded model:
  - request `(sub, dom, obj, act)`;
  - role definition `g = _, _, _` (user, role, domain);
  - domain is a space ID or `*` (global);
  - the matcher allows global grants to apply to every space.
  The built-in roles `viewer`, `operator`, `admin` and their permission policies are seeded and versioned by migration. Objects:
  `space, node, terminal, terminal.takeover, recording, secret, account, queue, inbox, human_action, approval, template, subscription,
  replay, mcp, skill, kb, memory, prompt, backup, user, token, audit, admin`.
  Grant rows (user/token → role, domain) live in SQLite and are loaded through a small store-backed adapter (`internal/authz`). The
  adapter reloads policy after committed grant changes (AfterCommit). Every API operation declares exactly one `(obj, act)`. Handlers
  never hand-roll checks.
  API-token scopes are expressed as Casbin subjects: a token gets its own subject whose roles are a subset of its creator's.
- **Consequences:** The policy is inspectable, testable and documented. Agent/run identities are **not** Casbin users; they get scoped
  run credentials checked by the MCP gateway against space/role grants (see ADR-0012).

### ADR-0005 — Web UI on Mantine 8 with a Night Owl theme, TanStack Router/Query/Store/Form/Table
- **Status:** accepted (operator direction, 2026-11-02)
- **Context:** The operator asked for the component base that fits best among the sibling projects. Pulse, Porch and Deed all use
  Mantine 8.3.18 with TanStack Router 1.170.46, Query 5.104.5, Store 0.11.2, Table 8.21.3, Virtual 3.14.22 and openapi-fetch 0.17.0,
  on React 19.3.1. Hallpass uses Ant Design. The brief asks for a Night Owl-based design. The operator also asked for TanStack Form and TipTap.
- **Decision:** The same pinned versions as Pulse, plus TanStack Form, ghostty-web and TipTap (open-source extensions only). Mantine theme
  tokens come from the Night Owl palette (dark default, Light Owl for the light scheme), with a contrast test for WCAG AA. Production
  embeds the UI through the `embedui` build tag (Hallpass pattern).
- **Consequences:** Shared conventions across the author's projects. Mantine's CSS-variable theming makes the Night Owl identity cheap.

### ADR-0006 — Node connection: outbound WebSocket, versioned JSON protocol, execution leases with fencing
- **Status:** accepted
- **Context:** SM-NODE-001…009. Nodes may sit behind NAT. Central is the single authority.
- **Decision:**
  - **Transport.** `spaceman-agent` dials `wss://<central>/agent/v1/connect` (TLS required except on loopback in dev). It uses `coder/websocket`.
  - **Enrollment.** A one-time enrollment token (shown once, HMAC-stored, short TTL) is exchanged for a node credential `spaceman_nd_…`.
    Central stores only its verifier. Rotation issues a new credential with overlap, and revocation closes the live connection.
  - **Protocol.** `spaceman.node/v1`. Every frame is `{v, id, type, ts, …}`. Commands carry `command_id` (stable), `space_id`, `run_id`,
    `generation`, `idempotency_key`. The node acknowledges each command as `accepted|duplicate|rejected(reason)` and later reports
    observed state. Hello carries the agent version, protocol range, capability inventory (runner and harness versions, git, OS/arch)
    and capacity.
  - **Lease and fencing.** Each space has a central `execution_lease{space_id,node_id,generation,expires_at}`. Every run command and
    every node report carries the generation, and anything that does not match is fenced (rejected and audited).
  - **Renewal and disconnect.** Heartbeats renew leases in bulk. On disconnect, the node stops accepting authoritative operations,
    keeps a grace timer (default 2 min), then interrupts and stops runs per the space's disconnect policy. Recording and the journal continue.
- **Consequences:** Central restarts never duplicate work. Runs whose node is disconnected show `unknown`.

### ADR-0007 — herdr-core is the runner; launch idempotency via terminal name and labels
- **Status:** accepted; contract details in `docs/contracts/herdr-core.md`
- **Context:** `hc` 0.1.16 exposes `terminal.run{name,labels,env,cwd,parent}`. Names are unique among live terminals (`name_taken`).
  `terminal.list{labels}` filters. `server.restart` adopts running programs. Status comes from OSC 7501, which Claude Code and Codex
  both emit natively.
- **Decision:**
  - **Own core.** The node runs its own hc core with `HC_SESSION=spaceman-<node>` and private XDG dirs under the agent data dir.
    It never uses the operator's default core.
  - **Launch.** Each run gets terminal name `r-<run short id>` and labels `spaceman.run`, `spaceman.space`, `spaceman.gen`. The launch
    sequence is: journal intent → `terminal.list{labels:{spaceman.run}}` → adopt or `terminal.run` → journal the observed address and
    pid. `name_taken` resolves to adoption.
  - **Status.** Agent status comes from hc status and turn events, never from screen text.
- **Consequences:** Adoption works across both node and hc restarts.

### ADR-0008 — Harness isolation: per-space, per-instance native config dirs
- **Status:** accepted
- **Decision:**
  - **Config dirs.** Claude Code runs with `CLAUDE_CONFIG_DIR=<data>/spaces/<space>/harness/claude/<instance>`. Codex runs with
    `CODEX_HOME=<…>/codex/<instance>` and always passes `--no-daemon`.
  - **Launch flags.** The harness launches interactively inside hc. SpaceMan MCP is configured with Claude `--mcp-config <file>
    --strict-mcp-config` or Codex `-c mcp_servers.spaceman…`. Both point to the local stdio bridge `spaceman-agent mcp-bridge`.
    Session identity is fixed with Claude `--session-id <uuid>` (resume with `--resume`) and Codex `resume <id>`, the id being
    discovered from `CODEX_HOME/sessions`.
  - **Leakage.** No user-level config is shared across spaces.
- **Consequences:** Leakage tests only need to check directory scoping and the env passed to each run.

### ADR-0009 — Secret delivery to runs through a launcher shim
- **Status:** accepted (2026-11-02). K-2 verified: hc persists `terminal.run.env` in plaintext as `restart_env` in `terminals.json`
  (0644), so the "if hc persists env" branch below applies.
- **Context:** SM-ACCT-004 asks for runner persistence to be inspected before choosing a delivery mechanism. hc keeps
  `terminals.json`, and whether it stores `env` is unverified.
- **Decision (pending verification):**
  - **If hc persists env.** Never put secrets in `terminal.run.env`. The run argv is `spaceman-agent launch --run <id> -- <harness argv>`.
    The shim reads secrets from `<run dir>/secrets.json` (0600, tmpfs where available), unlinks it, exports `CLAUDE_CODE_OAUTH_TOKEN`
    or `OPENAI_API_KEY`/`CODEX_API_KEY` etc., and then `exec`s the harness.
  - **If hc does not persist env.** Env may be passed directly, though the shim stays for uniformity.

### ADR-0010 — Recording from the node via hc attach (observe) into zstd segments
- **Status:** accepted (2026-11-02). K-1 was decoded from real traffic (spike S-3 in `docs/contracts/herdr-core.md`). The PTY-tee fallback
  is not needed.
- **Decision:**
  - **Capture.** The node opens a framed hc connection, calls `terminal.attach{observe:true}` and writes each DATA frame plus resize and
    lifecycle events as JSONL records `{seq, t_us, kind, data_b64|cols,rows|state}`.
  - **Segments.** Records go into zstd segments of at most 8 MiB or 5 minutes. Each flush (≤1 s) is a complete zstd frame, so a partial
    segment decodes up to its last flush.
  - **Gaps.** A reattach after `missed` or a restart emits a `gap` record.
  - **Upload.** Closed segments are uploaded to central storage.
  - **Fallback.** If the frames cannot be decoded reliably, a PTY tee shim inside the terminal records instead. A decision update would record that switch.

### ADR-0011 — Pulse ingestion: one durable consumer per subscription generation
- **Status:** accepted; contract details in `docs/contracts/pulse.md`
- **Context:** In go-pulse v0.1.3, `Subscribe(Durable(name))` creates `<clientID>_<name>` with a single filter, and binds an existing
  consumer **without updating its config**. Grants are `durable:subject:name`. JetStream dedup lasts only 2 minutes.
- **Verified (K-4, `internal/natsauth/grants.go` @ v0.1.3):** durable grant names are exact (`^[a-z0-9][a-z0-9_-]{0,47}$`, no patterns).
  A `durable:<subject>:<name>` grant permits `$JS.API.CONSUMER.CREATE.<stream>.<clientID>_<name>.<subject>`, plus INFO, MSG.NEXT,
  **DELETE**, ACK and FC for that consumer only. The grant subject is a NATS permission pattern, so a narrower filter inside it is permitted.
- **Decision:**
  - **Durables.** Each space subscription has its own **fixed** durable name, `sm-<subscription short id>` (at most 48 chars), so fan-out
    is natural (SM-EVT-012). The UI and API show the exact grant the Pulse operator must add.
  - **Cursor.** "Continue" keeps the consumer. "Start now" or "replay from T/seq" **deletes and recreates** the same-named consumer with
    `OnlyNew` or `Since(T)`/`FromSeq(n)`. The grant allows the delete, and because the consumer is gone the SDK applies the new start
    position. That edit is audited, and replay is labeled distinctly from retry.
  - **Filter changes.** Changing the effective filter outside the granted subject needs a new grant, and SpaceMan validates this before activation.
  - **Handler.** The handler validates, renders and inserts the inbox item in one write transaction, with
    `UNIQUE(subscription_id, event_id)` where event_id is `Nats-Msg-Id`. It returns nil (ack) only after commit. Invalid events are
    recorded in quarantine first and then returned as `Terminal`.
  - **Transient errors.** These return an error, which naks with SDK backoff.
  - **Templates.** The GitHub template's `repository` variable becomes a payload filter on `repository.full_name`, because the
    go-pulse-github subject does not contain the repo.
- **Consequences:** Operators grant one durable per subscription. Spaces never share a consumer.

### ADR-0012 — Agent identity and MCP: run-scoped credentials via the node bridge
- **Status:** accepted
- **Decision:**
  - **Credential.** Each run gets a run credential (random, HMAC-stored) bound to space, run, instance and role, valid while the run
    lease is valid.
  - **Bridge.** Harnesses talk MCP to `spaceman-agent mcp-bridge` over stdio. The bridge forwards to central over the node connection,
    attaching the run credential.
  - **Checks.** Central authorizes every tool call against the run's role grants. It never uses Casbin user policy for agents and never
    takes the model's word for anything.
  - **Upstream.** Credentials for upstream MCP servers stay central.

### ADR-0013 — Attachments stored as artifacts, bodies as Markdown, TipTap composer
- **Status:** accepted (operator addition, 2026-11-02)
- **Decision:**
  - **Storage.** Attachments are artifacts (`att_…`) in the artifact store (local FS or S3), content-addressed by sha256 and tracked per
    space for quotas.
  - **Upload.** Upload is a separate idempotent multipart call. Items reference attachment IDs. Unreferenced uploads are garbage-collected
    after 24 h.
  - **Bodies.** Item bodies are Markdown. The TipTap composer serializes to Markdown. The server never accepts HTML bodies.
  - **Serving.** Downloads are authorized against the owning space with `Content-Disposition: attachment`. Only png, jpeg, gif and webp
    images are allowed inline. Responses carry `X-Content-Type-Options: nosniff` and `Content-Security-Policy: sandbox`.
  - **Implementation notes (M2-01).** Content is written before the database transaction (no I/O inside it). Garbage collection
    removes unreferenced rows after 24 h, then content no row points to, skipping content touched in the last hour so a
    concurrent upload of the same bytes is never lost. Only the local backend exists; central refuses `artifacts.backend: s3`
    until it is implemented (M4). Per-space quota: `artifacts.space_quota_bytes` (1 GiB); per-upload limit: `http.max_upload_bytes`.

### ADR-0014 — Memory sync via git bundles over the node protocol
- **Status:** accepted
- **Decision:**
  - **Layout.** The node keeps one bare repo per (space, kind) and an instance worktree per agent instance.
  - **Checkpoints.** Checkpoints are debounced commits (default 30 s quiet, at most 5 min).
  - **Sync.** The node sends `git bundle` ranges with an expected parent. Central applies them to its bare repo only as a fast-forward.
  - **Conflicts.** On conflict, central rejects and the node keeps `refs/spaceman/conflicts/<ts>` locally. The conflict is reported for
    reconciliation (a merge commit by an agent or operator). No force updates ever.

### ADR-0015 — Outbound events: transactional outbox relayed to Pulse
- **Status:** accepted
- **Decision:**
  - **Outbox.** Outbox rows (`evt_…`) are written in the same transaction as the state change. A single relay publishes them with
    `Publish(WithID(evt id))`, marks a row done only after the PubAck, and retries with 2^(n-1) s ±50% backoff capped at 5 min, dead-lettering after 24 h.
  - **Subjects and types.** Subjects are `evt.spaceman.<area>.<entity>.<action>` and types are `spaceman.<area>.<entity>_<action>.v1`
    (Porch convention).
  - **Correlation and causation.** Pulse defines no contract for these, so SpaceMan defines its own extension headers,
    `spaceman-correlation-id` and `spaceman-causation-id`, and documents them as SpaceMan-specific.

### ADR-0016 — Docs site: VitePress 1.6.4 + Mermaid via vitepress-plugin-mermaid (Porch setup)
- **Status:** accepted
- **Decision:** Use the Porch setup: `vitepress-plugin-mermaid` 2.0.17, `mermaid` 11.17.2, a `check-mermaid` script, base `/go-spaceman/`,
  and a `Shot` component for screenshots taken by Playwright from a seeded demo instance.

### ADR-0017 — Dead-daemon enforcement via a per-space lease-guard parent terminal
- **Status:** accepted (2026-11-02), based on spike S-2 in `docs/contracts/herdr-core.md`
- **Context:** SM-NODE-008: a dead `spaceman-agent` cannot enforce its own disconnect timeout. hc ends child terminals when their parent's
  program ends, unless `outlive_parent` is set.
- **Decision:**
  - **Guard terminal.** For each space with active runs, the node starts a guard terminal `g-<space short id>` (`no_parent:true`) running
    `spaceman-agent lease-guard --lease-file <space run dir>/lease.json`.
  - **Daemon side.** The daemon rewrites the lease file atomically. Its `valid_until` is the central lease expiry plus the disconnect grace.
  - **Guard side.** The guard checks the file every second, uses only local time and the file, and exits when `now > valid_until` or the
    file is missing or invalid.
  - **Runs.** Every run terminal is started with `parent: g-<space>`, never with `outlive_parent`.
  - **Restart.** When the daemon restarts it adopts the guard and its children if both are live. If the guard ended, the runs ended with it,
    and the daemon reconciles them as stopped by lease expiry.
- **Consequences:** Disconnect enforcement holds even if the daemon crashes or is SIGKILLed, as long as the hc core and holder are alive.
  Documented limits:
  - It does not hold if hc itself is killed (the programs die anyway, `holder_lost`).
  - It does not hold if a node operator edits the lease file.
  - Ending a terminal does not prove that processes spawned outside hc stopped.
  - The graceful path, where the daemon is alive, sends `terminal.interrupt` first, then `kill`, per the space's disconnect policy.

### ADR-0018 — Foundation packages derived from Hallpass
- **Status:** accepted (2026-11-02)
- **Context:** The brief asks SpaceMan to follow Hallpass's actual implementation for storage, keyring, audit and observability, and to prefer
  sibling repositories for exact conventions. Hallpass is Apache-2.0 by the same author, and its foundations are tested and security-reviewed.
- **Decision:** These packages are adapted from go-hallpass v0.7.0 (`bdd8329`) with renamed identifiers, SpaceMan migrations and SpaceMan metric
  families, rather than rewritten: `internal/{store,keyring,audit,ids,clock,buildinfo,obs,config}` and `tools/analyzers/txscope`.
  Inherited comments that cite Hallpass requirement IDs are marked "Hallpass HP-…". Attribution is in NOTICE.
- **Consequences:** Same crypto, transaction and redaction behavior as Hallpass, including its tests. Bug fixes in either project should be
  considered for the other.

### ADR-0019 — Root-token bootstrap, scoped API tokens, MFA for privileged grants
- **Status:** accepted (2026-11-02). **Security-relevant change** relative to Hallpass, reviewed as follows.
- **Context:**
  - SM-AUTH-005 requires a full-scope, non-expiring root token, shown once. Hallpass instead issues a 30-minute setup token and has no root token.
  - SM-AUTH-006 requires API tokens with scopes. Hallpass gives each token exactly one role.
- **Decision:**
  - **Bootstrap.** `spaceman admin bootstrap` runs offline, under the data-directory lock and with the KEK. It issues the root token `spaceman_rt_…`
    (HMAC-stored, hint only), which holds `admin` in `*`.
  - **First admin.** The root token creates the first administrator, through the API or the `/setup` UI.
  - **Rotation and revocation.** Rotation is available online (global admin or root) and offline (CLI). Revocation is refused while no enabled
    global administrator exists. Every step is audited.
  - **What root cannot do.** Root cannot issue API tokens, since it is not a user. Root authority does not bypass service-level invariants.
  - **API tokens.** Each token carries its own grant set, and each grant must be covered by the creator's grants (`authz.Covers`). Requests are
    authorized as both token and creator (`AllowedAll`), so a token loses whatever its creator loses.
  - **MFA.** TOTP is mandatory for local accounts holding any operator or admin grant, and for all local accounts when `auth.require_totp` is on
    (the production default).
- **Consequences:**
  - The root token is a standing full-authority secret. Operators are told to store it offline and revoke it after creating administrators.
    The docs and the `/setup` flow say so.

### ADR-0020 — Web UI content security policy allows WebAssembly for the terminal
- **Status:** accepted (2026-11-02). **Security-relevant**: it relaxes the Pulse/Hallpass UI policy in two directives.
- **Context:**
  - The live and replay terminal uses ghostty-web 0.4.0 (MIT), which compiles Ghostty's VT parser as WebAssembly.
  - The package inlines the module as a `data:application/wasm;base64,…` URL. It fetches that URL and calls `WebAssembly.instantiate` (verified in
    `dist/ghostty-web.js`).
  - The Pulse/Hallpass UI policy (`script-src 'self'`, `connect-src 'self'`) blocks both steps.
- **Decision:** UI pages (not the API, whose policy stays `default-src 'none'`) are served with
  `default-src 'self'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:;
  connect-src 'self' data:; worker-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'`.
  - `'wasm-unsafe-eval'` allows compiling WebAssembly only. JavaScript `eval` and inline scripts stay forbidden, and `check-dist` fails a build
    that contains an inline script.
  - `connect-src data:` lets the page fetch its own inlined module. A `data:` URL carries no credentials and reaches no server, so it cannot be
    used to exfiltrate data to another origin.
  - `style-src 'unsafe-inline'` is inherited from Pulse and Hallpass (Mantine sets inline styles).
- **Consequences:**
  - Terminal output is untrusted program output. It is rendered by the VT parser onto a canvas, never as HTML.
  - The terminal is watch-only (`disableStdin`): keystrokes in the page are never sent to the run. Operator input goes through the separate,
    permission-checked `send-input` operation.

### ADR-0021 — Claude first-run state and folder trust are seeded for the run's directory only
- **Status:** accepted (2026-11-02). **Security-relevant**: SpaceMan answers Claude Code's folder-trust prompt.
- **Context:**
  - Each space and instance has its own `CLAUDE_CONFIG_DIR` (ADR-0008). A fresh one stops at the first-run theme picker and
    then at "Is this a project you trust?" (spike S-5). An unattended run would wait forever.
  - The run's working directory is created and owned by SpaceMan inside the space folder; operators chose to run an agent
    there when they launched the run.
- **Decision:**
  - Before a launch, the node agent merges into `$CLAUDE_CONFIG_DIR/.claude.json`: `hasCompletedOnboarding: true`, a
    theme, and `hasTrustDialogAccepted: true` for exactly the run's working directory (symlinks resolved). Nothing else in
    the file is changed, and no other directory is trusted.
  - The launcher shim removes inherited Claude Code and Codex session markers and node-level provider credentials
    (`ANTHROPIC_*`, `CLAUDE_CODE_*`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `CODEX_ACCESS_TOKEN`) before applying the run's
    own secrets, so a run uses only its SpaceMan account (SM-ACCT-004).
- **Consequences:**
  - Trusting the directory lets Claude Code read project settings and hooks committed in that repository. That is the
    same trust an operator gives by launching an agent there; the permission mode still governs tool use.
  - Keys are taken from observed 2.1.316 behaviour, not a documented API; a harness upgrade is re-verified by the real suite.

### ADR-0022 — Agent tools travel over the node connection; one sealed credential per run
- **Status:** accepted (2026-11-02). **Security-relevant**: run tokens are now also stored encrypted.
- **Context:**
  - ADR-0012 puts agents behind `spaceman-agent mcp-bridge`, forwarding to central over the node connection.
  - Launch commands are re-sent until the node acknowledges them. Issuing a new token on every send revoked the token
    the running harness already held, so its tool calls failed (found by `TestAgentToolsOverBridge`).
- **Decision:**
  - Node protocol minor 1 adds `tool.call` and `tool.result`. The node forwards a call only for a run whose token it
    issued; central authenticates the token itself, checks that the run belongs to that node, and runs the tool with a
    25-second deadline and at most 8 concurrent calls per node. The bridge waits at most 30 seconds.
  - The bridge asks central for the run's catalog (`_catalog`) and serves exactly those tools. Grants are by run kind,
    in code (`internal/agenttools`); arguments can never select another space or requester.
  - A run has one token for its lifetime. It is stored as a MAC (authentication) and sealed with the keyring purpose
    `credential` (re-delivery). Ending the run revokes it.
- **Consequences:**
  - A keyring compromise exposes active run tokens; they grant only that run's tools while it is active.
  - Internal errors reach the agent as "internal error"; details are logged centrally.

### ADR-0023 — The MCP gateway gives each run its own upstream session and dials only allowed addresses (security-relevant)
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-MCP-001/002 require external MCP servers to be reached through SpaceMan: agents never get their
  address or credentials, grants are per space and role, and upstream sessions must not be assumed stateless or safe to
  multiplex. An upstream address chosen by an administrator could also point at internal services (SSRF).
- **Decision:**
  - Servers are registered centrally (streamable HTTP only). Request headers are sealed with the keyring purpose
    `credential` and are write-only; only their names are returned.
  - A grant names a space and a space role (or `*`) and optionally a subset of tools. A server may also restrict the
    tools it offers. Tools are exposed as `<slug>__<tool>`; slugs are unique and cannot contain `__`, so names never
    collide with each other or with SpaceMan's own `spaceman_*` tools. Names longer than 64 characters are not offered.
  - Agent calls travel the existing tool path (ADR-0022). Central opens **one upstream session per run and server**,
    never shared or reused by another run, closed when the run ends, the grant is revoked, the server changes, after
    10 idle minutes, or after a transport failure. At most 256 sessions are open at once.
  - Every call re-reads the grants. A per-server call timeout (≤ 20 s), a per-server concurrency limit across runs, an
    argument limit (256 KiB) and a result limit (≤ 4 MiB, enforced on the HTTP body as well) apply. Each call is audited
    with run, server, tool and outcome, never arguments or results.
  - The gateway client declares no client capabilities: upstream servers cannot ask the agent for sampling,
    elicitation or roots. The standalone SSE stream is disabled.
  - Egress: the dialer checks the address actually connected to, after DNS resolution. Link-local, multicast,
    unspecified and broadcast addresses are always refused. Loopback, RFC 1918, CGNAT, unique-local IPv6 and the IPv6
    translation prefixes are refused unless the server is marked `allow_private` by an administrator; plain http is
    accepted only then. Environment proxies are ignored and redirects are never followed.
- **Consequences:**
  - Upstream state is per run: a relaunched run starts a fresh upstream session.
  - Tools are listed when the harness starts its MCP bridge; tools granted later appear on the next launch, while a
    revocation applies on the next call.
  - Results are passed to the agent as the upstream sent them and are untrusted content (prompt injection risk).
  - Configured approvals for gateway calls arrive with the approval gateway (M3-05).

### ADR-0024 — Enforced approvals gate SpaceMan's own operations without waiting (security-relevant)
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-APPR-001…005 distinguish advisory approvals from enforced ones, which bind the exact operation,
  resource, arguments, requester, expiry and uses, recheck authorization, prevent replay and keep approval state apart
  from execution state. Agents must not hold MCP calls open waiting for people.
- **Decision:**
  - Enforcement covers operations SpaceMan performs: built-in agent tools (`spaceman.<tool>`) and gateway tools
    (`mcp.<server>.<tool>`). Policies (global administration) name operations exactly or by a `.*` prefix, per space or
    for every space. The tools needed to ask and talk to people can never be gated.
  - A gated call without a matching approval returns at once with an approval request: an `approvals` row and a human
    action of type `approval` (created with a dedup key, so a crash between the two is repaired on the next call). The
    agent is notified of the decision and calls again.
  - An approval matches only the same operation, resource (MCP servers by ID), space, requesting **run** and digest:
    SHA-256 over the operation, resource and canonical JSON arguments (sorted keys, numbers kept exactly).
  - Deciding needs `approval:decide` (API). Before every execution the approver is checked again (user active, API
    token unrevoked or root token current, policy still granting `approval:decide`); otherwise the approval is revoked.
  - One use is taken and the execution recorded in a single write transaction with a conditional decrement, so
    concurrent calls cannot overrun the uses. Executions (`executing`, `succeeded`, `failed`, `interrupted`) are
    separate from the decision (`pending`, `approved`, `denied`, `expired`, `revoked`). Executions found running at
    start-up are marked interrupted and never retried.
  - Previews redact secret-looking fields; the digest covers the real values; arguments are never logged or audited.
- **Consequences:**
  - A relaunched run needs new approvals for the same operation.
  - Shell commands and harness-native permission prompts are not covered.
  - An interrupted execution may or may not have taken effect; people decide whether to approve it again.

### ADR-0025 — Role memory is Git history owned by the node and preserved centrally by fast-forward only
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-MEM-001…006: memory keyed by space and role, shared by every instance, one bare repository per
  collection with instance checkouts on the node, debounced checkpoints, bounded asynchronous sync, parent revisions for
  concurrency, conflicts preserved and reported (never overwritten or force-pushed), central guaranteeing only
  acknowledged history, and central edits following active ownership.
- **Decision:**
  - The node keeps `<data>/memory/<space>/<role>/shared.git` and `instances/<instance>` checkouts and runs the `git`
    command line with hooks and fsmonitor disabled, no system or global config and repository paths instead of
    remotes (agents can write their checkout's `.git`).
  - Checkpoints happen 15 seconds after a run reports idle or done, and at once when a run ends. A checkpoint publishes
    to the shared history by fast-forward or a clean merge. On a conflicting merge the shared version wins; the
    instance's history is pushed to `refs/conflicts/<instance>-<time>` and its versions of the conflicting files are
    committed under `conflicts/`, for the role to reconcile.
  - Within 5 seconds of a publish (backoff up to 10 minutes on failure; again on reconnect) the node sends a Git bundle
    from the last acknowledged head (`memory.sync`, node protocol minor 2, at most 2 MiB). Central fetches it into
    the collection's bare repository and moves `main` only if the new head descends from central's acknowledged head
    (compare-and-swap on the ref and the database). Otherwise it keeps the history under `refs/conflicts/node-<time>`
    and answers `conflict`; a bundle on a base central lacks is answered `missing_base` and resent in full.
  - Only the space's assigned node may sync a space's memory. Central offers read-only files and history; marking a
    conflict reconciled is its only write.
  - The node reports per-collection status (`memory.status`): uncheckpointed files, unsynced commits, pending sync,
    last acknowledgement, conflicts, retries and the last error.
- **Consequences:**
  - Central needs `git` to store memory; without it, syncs are refused and the UI says so.
  - Changes newer than the last acknowledgement exist only on the node.
  - Rewritten history (e.g. after a restore) is preserved but not applied; reconciling it is a person's decision.

### ADR-0026 — KB support runs as per-requester kb sessions; KB changes are proposals accepted on the node
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-KB-001…007 and SM-CTRL-005: optional KB repositories; any permitted agent asks; one support session
  per requester with continuity, limits, queuing, cancellation, timeouts, idle sleep, recovery and cleanup;
  correlation; mixed harnesses; concurrent KB writes in separate worktrees with serialized or reviewed acceptance and
  preserved conflicts; KB sessions must not orchestrate workers or change policy; no MCP call may wait for an answer.
- **Decision:**
  - A KB is configured per space (repository folder, knowledge branch, the space role its sessions run as — a template
    of kind `kb` —, entry instructions, an optional validator command, write policy `read_only` / `review` /
    `serialized`, admitted requester roles, session limits). Admission is that configuration: asking is a scoped
    operation, not permission to launch runs.
  - Each requesting agent **instance** gets one session: its own `kb` run, instance (harness configuration and
    conversation) and worktree on `kb/<kb>/<session>`. Sessions run within the KB's own `max_sessions` pool, separate
    from workers, so workers waiting on KB answers cannot starve it; further requests queue.
  - `spaceman_kb_ask` returns at once. One scheduler goroutine (woken by events, one timer for the nearest deadline)
    dispatches each session's requests one at a time as durable SpaceMan messages, starts or resumes sessions
    (resuming the harness conversation), times requests out, puts idle sessions to sleep by stopping their run, marks
    sessions whose run died as failed and offers their request again (at most twice), and closes old sessions.
  - The session answers with `spaceman_kb_answer` (basis `committed`, `proposal` or `none`, and evidence: paths and
    revisions); the requester is notified by message and reads it with `spaceman_kb_get`. Claude's SendMessage is not
    used (unverified across separate runs); the same tools work for Claude Code and Codex.
  - KB sessions commit only on their branch and offer commits with `spaceman_kb_propose`. Acceptance is a node command
    (`kb.accept`, protocol minor 2): the commit must be on the session's branch; the knowledge branch moves by
    fast-forward or a clean merge built with `merge-tree`/`commit-tree` (no checkout, no hooks), only after the
    validator passed in a temporary worktree, and only by compare-and-swap; a branch checked out anywhere is never
    moved; conflicts change nothing and are reported. Review KBs wait for a person; serialized KBs accept one proposal
    at a time. Results come back as `kb.result` and are resent after reconnects.
  - KB sessions get no orchestration tools; the KB repository, role memory and the support conversation stay separate.
- **Consequences:**
  - The validator runs on the node with the node user's rights, like agent programs; configuring it needs kb:write.
  - A session resumed after sleep continues its conversation only where the harness supports resume.
  - Knowledge branches must not be checked out where SpaceMan should move them.

### ADR-0027 — Dreams consolidate memory through ordinary checkouts; evidence advances only after a checkpoint
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-DREAM-001…004: a dreamer is a normal agent on the space's node; it reads permitted transcripts,
  memories, KB repositories and instructions without administrative APIs or recordings; scheduled and manual runs with
  time, token and evidence budgets; bounded discovery; evidence positions advance only after results are durably
  checkpointed; its own transcripts are excluded by default; a set prompt method; findings go to control, KB or human
  actions; a no-change dream is a success.
- **Decision:**
  - A dream configuration names a space role whose template is of kind `dreamer`, the role memories it consolidates,
    whose transcripts it may read (its own role excluded unless configured), an interval (0 = manual), a time budget
    (enforced: the run is stopped and the dream is `timed_out`), a token budget (told to the dreamer; not enforced) and
    an evidence budget (files and bytes). One scheduler goroutine with one timer starts due dreams and enforces budgets;
    one dream per configuration runs at a time; the configuration keeps one dreamer instance across dreams.
  - The node prepares `<space>/dreams/<run>`: the dreamer instance's ordinary checkouts of the target memories
    (`memory/<role>`), copies of transcript bytes written since the configuration's committed position (Claude Code
    `projects/**/*.jsonl` and Codex `sessions/**/*.jsonl` under the space's instances; at most 5000 entries walked;
    newest first, within the budget), and `DREAM.md`. The positions read are journaled as pending.
  - The dreamer reports with `spaceman_dream_report` (summary, changes, findings, `no_change`) and publishes findings
    with the ordinary tools. Central then sends `dream.commit`: the node checkpoints and publishes each memory checkout
    first and only then commits the pending positions; its ack marks the dream `succeeded` (or `failed`) and central
    stops the run. A dream that ends without reporting, or runs out of time, advances nothing.
  - The method is SpaceMan's own built-in prompt `dream-consolidation` (seeded in the prompt catalog): orient, search
    narrowly, consolidate, correct contradictions, preserve provenance, absolute dates, compact indexes, conservative
    pruning, flag instruction conflicts, never rewrite policy, never promote personal notes into authority.
- **Consequences:**
  - Dream memory changes merge with other instances' like any checkpoint; conflicts are preserved (ADR-0025).
  - Copies of transcript segments live in the dream folder on the node; they are as sensitive as the transcripts.

### ADR-0028 — Usage comes only from harness records; transcripts are stored apart from recordings
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-REC-005 and §30: keep recordings, native transcripts and normalized metadata distinct; normalize only
  available data; unknown usage is unknown, not zero; estimates keep their pricing assumptions; do not claim hard
  currency limits when usage is delayed.
- **Decision:**
  - The node reads usage from the harness's own records: for Claude Code, its session transcript (spike S-7). Token
    counts are summed per model with streamed records counted once (last record per `message.id`). Claude Code's own
    `cost-state` estimate is kept as an estimate with its source and assumptions ("API list prices; subscription
    accounts are not billed per token"). SpaceMan has no price table of its own.
  - Codex's records are unverified, so its usage is reported with source `unavailable`; the API and UI show
    "unknown" and space totals count such runs separately instead of adding zero.
  - Usage is reported after idle periods (debounced 30 s) and finally when the run ends (`usage.report`); a final
    report is never overwritten.
  - After a run ends, the node uploads its native transcript (at most 64 MiB; longer ones keep their start, marked
    truncated) in 1 MiB chunks, stop-and-wait, resumable from central's offset after reconnects (`transcript.chunk` /
    `transcript.ack`); central verifies the SHA-256 before storing it under `transcripts/`. Transcripts are downloads
    only, need recording permissions and can be deleted.
- **Consequences:**
  - Usage during a run is "so far"; budgets that need exact spend cannot be enforced from it.
  - Transcripts contain whatever the session saw, including possible secrets.

## 2. Open items and unresolved contracts

| ID | Item | Blocks | Plan |
|---|---|---|---|
| K-1 | ~~hc framed protocol byte layout.~~ **Resolved 2026-11-02:** `ch u32 LE · kind u8 · len u32 LE`; DATA=0, CREDIT=1, CLOSE=2, RESIZE=3 (spike S-3). | — | done |
| K-2 | ~~Whether hc persists `env`.~~ **Resolved 2026-11-02:** yes, plaintext `restart_env` → ADR-0009 shim required. | — | done |
| K-3 | ~~hc parent semantics.~~ **Resolved 2026-11-02:** children killed when the parent program exits or is killed, unless `outlive_parent` → ADR-0017 lease guard. | — | done |
| K-4 | ~~Whether Pulse `durable` grants accept name patterns.~~ **Resolved 2026-11-02:** exact names only; DELETE permitted → fixed name + delete/recreate (ADR-0011). | — | done |
| K-5 | ~~ghostty-web package and API.~~ **Resolved 2026-11-02:** `ghostty-web` 0.4.0 (MIT): `init()`, `new Terminal({cols, rows, theme, fontSize, fontFamily, disableStdin, scrollback})`, `open`, `write(string \| Uint8Array)`, `resize`, `reset`, `dispose`; WebAssembly is inlined (ADR-0020). | — | done |
| K-6 | ~~Official Go MCP SDK version and streamable/stdio APIs.~~ **Resolved 2026-11-02:** `github.com/modelcontextprotocol/go-sdk` v1.8.0: `mcp.NewServer`, typed `mcp.AddTool`, raw `Server.AddTool` (input schema must be an object, or it panics), `IOTransport` for stdio. Upstream client (M3-03): `mcp.NewClient` + `Client.Connect` with `StreamableClientTransport{Endpoint, HTTPClient, MaxRetries, DisableStandaloneSSE, MaxEventSize}`; the connection context is detached from the `Connect` context; `ClientSession.Tools` (paginated), `CallTool`, `Close`. Transport failures surface as JSON-RPC code -32005. | — | done |
| K-7 | ~~TipTap extension set.~~ **Resolved 2026-11-02:** `@tiptap/react`, `starter-kit`, `markdown` (official Markdown parse/serialize), `extension-image`, `extension-list` (task lists), `extension-table`, `extension-file-handler` are MIT (npm registry). Pinned to 3.32.0 (released 2026-10-13); renderer `marked` 18.1.1 (MIT) + `dompurify` 3.4.17 (MPL-2.0 OR Apache-2.0). | — | done |
| K-8 | ~~llm-knowledge-base-template layout and licence; Piebald dream prompts licence.~~ **Resolved 2026-11-02:** KB template in `docs/contracts/kb-template.md`. Piebald-AI/claude-code-system-prompts is MIT, but its files are extracted Claude Code prompts whose text the repository licence may not cover; SpaceMan's dream prompt is written from the brief's principles and copies none of it (ADR-0027). | — | done |
| K-9 | Codex ChatGPT-login token materialization (`auth.json`) vs API key. | SM-ACCT-002 | Inspect Codex docs/behaviour; support API key first, document ChatGPT auth handling. |
| K-11 | Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26). SpaceMan grants have no SSO source yet (OIDC lands in M4-01). | SM-AUTH-002 | When OIDC lands, store SSO grants under the subject `user:<id>#sso`, add that subject only for SSO sessions, and port the SR-26 test. |
| K-10 | ~~Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode.~~ **Resolved 2026-11-02:** works interactively with an empty config dir once first-run state and folder trust are seeded (spike S-5, ADR-0021). | — | done |
