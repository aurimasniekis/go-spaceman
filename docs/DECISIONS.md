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

### ADR-0029 — OIDC sign-in ported from Hallpass; SSO grants apply only to SSO sessions (security-relevant change)
- **Status:** Accepted, 2026-11-02.
- **Context:** SM-AUTH-001 asks for Hallpass's actual OIDC implementation; SM-AUTH-002 for Porch per its contract
  (`docs/contracts/porch.md`: discovery, PKCE S256 only, `porch.groups` via a dotted `groups_claim`, `porch.roles`
  always empty, stable `sub`). K-11 asked that SSO-sourced grants apply only to SSO sessions (Hallpass SR-26).
  Adding a second way to sign in and a second source of grants widens the authentication surface, so this is recorded
  as a **security-relevant change**.
- **Decision:**
  - `internal/oidc` is ported from go-hallpass `internal/oidc` at `bdd8329` (provider, flow, admin, remote, the
    in-process test IdP and the tests), with operators → users and `operator_identities` → `user_identities`
    (migration 0021). Unchanged from Hallpass: single-use 10-minute state bound to the browser by a cookie
    (`__Host-sm_oidc`), nonce, PKCE S256 always, ID tokens RS256/ES256/EdDSA only with iss/aud/azp/exp/iat/nbf/nonce
    checks and ≤ 60 s skew, a cached JWKS with rate-limited refetch on unknown key IDs, identity by `(iss, sub)` and never
    by email, no email merge for JIT users, JIT only with at least one matching rule (groups, verified email domain,
    `tid`, `hd`), access and refresh tokens discarded, upstream step-up with `prompt=login`, `max_age=0`, `auth_time` and
    optional `acr_values` (local TOTP fallback only where the provider allows it), self-link with recent
    authentication, admin link by subject, single-use link invitations (`link_code`, 72 h), test login with an
    in-memory preview, sanitized `return_to` (SR-24), audited denials.
  - Provider HTTP (discovery, JWKS, token): its own client with TLS verification always on (a CA bundle only adds
    trust), no environment proxy, no redirects, and a dial-time refusal of loopback, private, link-local, CGNAT,
    unspecified and multicast addresses unless the provider's `allow_private_network` is set; endpoints must be https
    (http on localhost only in development mode) on the issuer's registrable domain unless
    `allow_cross_origin_endpoints` is set. Client secrets are sealed with the keyring's credential purpose
    (AAD `oidc_provider:<id>`) and write-only in the API.
  - **Grants instead of Hallpass roles and groups.** A group mapping maps a claim value to a SpaceMan role in a domain:
    `*` or a space that exists (the mapping goes with the space). Mappings are re-applied at every SSO sign-in.
    The resulting grants are `role_grants` rows of the subject `user:<id>#sso` (source `sso`). Casbin treats that
    subject as holding its own grants plus the user's (`authz.SSOSuffix`); only SSO sessions act as it
    (`Principal.Subjects`). Password sessions, break-glass sessions and API tokens never get SSO grants, and tokens
    stay bounded by the user's own grants (K-11, SR-26). An approval decided in an SSO session is rechecked against the
    SSO subject (`audit.Actor.Via = "sso:<slug>"`).
  - SSO grants never count toward the last-administrator guard. Hallpass instead keeps an SSO administrator role when
    sync would remove the last administrator; SpaceMan does not need that, because a user with a manual global admin
    grant must always remain. Deleting a provider or unlinking an identity drops the user's SSO grants; unlinking also
    ends the user's sessions. A user cannot unlink their last way to sign in (a SpaceMan addition).
  - The authorization response's `iss` parameter (RFC 9207, which Porch sends) must equal the issuer when present
    (a SpaceMan addition against mix-up; Hallpass ignores the parameter).
  - The browser routes are API operations (`/api/v1/auth/oidc/{provider}/start`, `/api/v1/auth/oidc/callback`); the
    redirect URI is `<public_url>/api/v1/auth/oidc/callback`. Errors redirect to `/login` or `/account` with a stable
    `sso_error` code, never internals. Provider administration needs `user:write` in `*` with a recent step-up; the test
    login needs `user:write` through Casbin and a full (not MFA-pending) session.
- **Not ported (yet):** Hallpass's SSO-only enforcement (`local_login_disabled` switch and its SR-06 lockout guards),
  the "direct provider" sign-in setting and RP-initiated logout (`end_session_endpoint`). There is no switch that turns
  password sign-in off, so the guards have nothing to protect yet.
- **Consequences:**
  - A person's access can differ between their password session and their SSO session; the Users page shows SSO grants
    separately.
  - Porch behaviour is verified only against the in-process provider that mimics its contract; real Porch evidence is
    still missing (TRACEABILITY SM-AUTH-002).

### ADR-0030 — Outbound events: the outbox, its catalog and the loop guard (security-relevant change)
- **Status:** Accepted, 2026-11-03. Implements ADR-0015 (M4-03).
- **Context:** SM-OBS-004 asks for outbound publication through a transactional outbox using the verified Pulse contract
  (`docs/contracts/pulse.md`, go-pulse v0.1.3 `9aba53b`): stable IDs, versioned payloads, correlation and causation,
  at-least-once delivery, retry and backoff, lag and failure visibility, and safe metadata only for human actions.
  SM-OBS-005 keeps inbound subscriptions apart from publication and forbids publishing inside a write transaction.
  SM-EVT-016 asks that origin and causation prevent feedback loops. Publishing sends metadata about spaces, runs, tasks
  and people's requests to another system, a new egress channel, so this is recorded as a **security-relevant change**.
  It is off by default.
- **Decision:**
  - **Port.** `internal/outbox` is adapted from go-hallpass `internal/events` (its `event_outbox` recorder and relay,
    migration 0030, `queries/events.sql`) and the wake pattern of its `internal/outbox`, at `bdd8329` (Apache-2.0, same
    author). Migration `0022_outbox` adds `event_outbox` with `space_id`, `correlation_id` and `causation_id`.
  - **Enqueue in the change's transaction.** Services call `Outbox.Enqueue(ctx, tx, Event)` inside the write transaction
    that makes the change (runs, tasks, inbox items, human actions, approvals, memory, KB). It only inserts a row and
    registers `tx.AfterCommit(Kick)`; nothing touches the network inside the transaction (the `txscope` analyzer
    allows it because it takes the `*store.Tx`). A rolled-back change leaves no event.
  - **Relay.** One goroutine (`Outbox.Run`) dials Pulse with `pulse.Connect(url, WithCredsFile, WithSource, WithCAFile)`
    (retrying every 30 s while that fails), then publishes due rows with `Publish(subject, type, payload, WithID(row id),
    WithTime(created), WithHeader(…))` and marks a row published only after the JetStream acknowledgement. It sleeps on
    the kick channel and one timer set to the earliest retry (an hour when nothing is pending): no polling. A gauge
    refresh runs every 30 s.
  - **Retries.** A failed publish backs off `2^(n-1) s × (0.5 + jitter)`, capped at 5 min; one failure ends the round.
    After 50 attempts or 24 h the row is dead-lettered (logged at error level, counted, shown). Every attempt uses the
    same ID, so JetStream drops a duplicate inside its 2-minute window and consumers deduplicate on `Nats-Msg-Id`
    beyond it. Delivery is **at least once**.
  - **IDs, names, versions.** The event ID is the outbox row ID (`evt_<26 base32>`, ADR-0002), not a ULID; Pulse does
    not validate IDs. Subjects are `evt.spaceman.<area>.<entity>.<action>` and types
    `spaceman.<area>.<entity>_<action>.v<n>` (Porch, `docs/contracts/porch.md`). Payloads are Go structs in the package's
    catalog; only that package can define one, so nothing outside the catalog is published. A non-additive payload
    change gets a new type version.
  - **Safe metadata.** Payloads carry identifiers, kinds, states, outcomes, priorities and commit IDs. Never titles,
    bodies, objectives, results, reasons, requests, responses, comments, prompts, approval arguments or resources, error
    messages or terminal output. Free-form identifiers (origin IDs, correlation IDs from templates) are published only
    when they match `^[A-Za-z0-9][A-Za-z0-9._:/#@+=-]{0,127}$`.
  - **Correlation and causation (SpaceMan extension headers).** `spaceman-correlation-id` names the work an event
    belongs to (the run, task, human action's correlation or task, approval, memory collection, KB proposal, or an inbox
    item's correlation). `spaceman-causation-id` names what caused it when SpaceMan knows it: the inbound Pulse event of
    an inbox item, a task's origin, the human action that decided an approval, the run that used it, the node's memory
    sync, the node command of a KB acceptance, or else the API request (`req_…`). Pulse defines neither.
  - **Loop guard.** Every event carries `spaceman-origin: <source>`. While publication is enabled, the inbound side
    acknowledges an event whose `spaceman-origin` equals the installation's own source and records it as `filtered`
    ("published by this SpaceMan"), so SpaceMan never turns its own events into inbox items. Each installation needs its
    own `source`.
  - **Configuration.** `events.outbound` (`enabled` false by default, `url` nats:// or tls://, tls:// required in
    production except loopback, absolute `creds_file`, optional `ca_file`, `source` default `spaceman`, `retention`
    default 7 days). While disabled nothing is enqueued; rows left pending from an enabled period are dropped after the
    retention. Published and dead-lettered rows are deleted after the retention by the hourly housekeeping.
  - **Visibility.** Prometheus `spaceman_events_publish_total{outcome}`, `spaceman_events_pending`,
    `spaceman_events_dead` and `spaceman_events_lag_seconds` (age of the oldest pending event); `GET
    /api/v1/system/outbound-events` (`system:read`, global admins) and the Events page's Publishing tab show the
    connection, pending, retrying and dead counts, lag, attempts since start, the last error and recent failures.
- **Alternatives:** publishing every audit record (as Hallpass does) would leak audit details such as reasons and
  arguments into the bus; a poll ticker (Hallpass, Porch) wakes the process every few seconds for nothing; publishing
  from `AfterCommit` directly would lose events on a crash.
- **Consequences:**
  - Consumers must deduplicate on `Nats-Msg-Id`; ordering between subjects is not guaranteed.
  - Events of a period with publication off are never published. Automation depth across installations (event →
    item → task → event) is not bounded yet; only an installation's own events are dropped.
  - Verified against the in-process Pulse-shaped NATS server (real nats-server JetStream and the real SDK); no evidence
    against a running `pulsed` yet.

### ADR-0031 — Sealed backups and a paused restore with new execution authority (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-04).
- **Context:** SM-STORE-003…006 ask for backups of the database, synced Git memory, catalog versions, subscription and
  configuration state, outstanding human actions, uploaded artifacts, transcripts, recordings and attachments and the
  encrypted credentials, with manifests, checksums, verification, scheduled and manual runs and retention; key recovery
  documented separately; and a restore that comes up paused with new execution authority, reconciles consumer positions
  conservatively and revalidates restored approvals. Central keeps recordings, transcripts, attachments and memory as
  plaintext files on its own disk, so a backup copied elsewhere would expose them. A backup also creates new copies of
  the keyring's wrapped keys and new egress (S3), so this is recorded as a **security-relevant change**.
- **Decision:**
  - **Port.** `internal/backup` is adapted from go-hallpass `internal/backup` at `bdd8329` (Apache-2.0, same author):
    the read-only `VACUUM INTO` source, the staged-and-renamed backup directory, the manifest with per-file SHA-256 and
    `manifest.sha256`, the verification walk (unlisted files, `integrity_check`, schema and KEK checks), the staged
    restore under the data-directory lock with `--force` moving data aside, the `HPBAK1` sealed object format and the
    remote layout. `internal/s3client` (and `s3test`) is ported unchanged apart from names. Hallpass's segment and anchor
    checks do not apply (SpaceMan recordings are not keyed segments).
  - **Everything is sealed.** Unlike Hallpass's local backups, every SpaceMan backup file is sealed (format `SMBAK1`,
    `sealed.go`): HPBAK1 with SpaceMan labels; ChaCha20-Poly1305 in 64 KiB frames with a final-frame flag, one random
    32-byte DEK per object wrapped by the KEK with associated data `spaceman/bak/dek/v1|<kind>|<backup_id>`, kind
    `file:<path>` or `manifest`, payload key HKDF-SHA256(DEK, salt). Standard-library and `x/crypto` primitives only; the
    construction has not had external review. `manifest.json` stays plaintext (metadata, no content) so backups can be
    listed and checked without the key; `manifest.json.smbak` authenticates it.
  - **Content.** The snapshot is read through a read-only connection (no lock, no writer blocked); then recordings,
    attachments (`artifacts/sha256/…`, not unfinished uploads), transcripts (including `.part` uploads in progress),
    one `git bundle --all` per central memory repository, and the effective configuration (YAML; no secret values) are
    sealed. Files removed between snapshot and copy are warnings. The KEK, `dev-kek`, node-side state and secrets
    referenced by path are excluded and every manifest says so.
  - **Targets and schedule.** `backups` configuration: `target` `local` (default `<data_dir>/backups`) or `s3` (keys from
    files, TLS verified, no redirects or proxy, private addresses refused unless allowed, objects uploaded with their
    SHA-256 checked, `manifest.sha256` last and deleted first). One scheduler goroutine with one timer (no polling): due
    an interval after the last success, five minutes after start when there is none, at least 15 minutes after a failure.
    One backup or verification at a time. Retention keeps `keep_last` complete backups and deletes those older than
    `max_age` except the newest, and incomplete leftovers after a day. Runs and verifications are recorded in `backups`
    (migration `0023`) and audited (`backup.started`, `backup.finished`, `backup.verified`, `backup.deleted`).
  - **Interfaces.** `GET/POST /system/backups` and `POST /system/backups/{id}/verify` (`system:read`, `backup:run`:
    global admins only), the Backups page, and `spaceman backup create|list|verify|restore`. Restore is CLI-only, offline,
    as in Hallpass: it needs the data-directory lock and the KEK; a backup is never restorable over the API. No download
    endpoint: backups are fetched from their target.
  - **Restore rules (SM-STORE-006).** Restore writes a pending marker into the restored database; the first start applies
    it once, in one transaction, audited as `backup.restored`, and keeps the result for the restore notice:
    - every active space is paused (`hold`);
    - every space's lease is revoked and its generation advanced by `LeaseGenerationStep` = 1,000,000, so the next grant
      exceeds anything the old installation could have issued after the backup; a node receiving it stops every run of
      another generation, and central fences reports and commands of older ones;
    - runs that were active become `unknown` with desired `stopped`; unacknowledged node commands are cancelled; every
      run credential is revoked (no pre-restore run can call tools);
    - every event subscription gets a new cursor that replays from its oldest stored delivery (`seq`), or from its
      creation (`time`) when no delivery with a sequence is stored, with replay marking off: the Pulse durable consumer
      may have been advanced by the old installation, and replaying with deduplication (inbox dedup key per
      subscription and event ID) can only duplicate deliveries, never skip events;
    - approvals that were approved with uses left are revoked (their use after the backup is unknown, and an approved
      operation must not run twice); pending approvals and open human actions are kept, and every use still rechecks the
      approver's authorization (SM-APPR-003) and the expiry; expired ones are expired by the usual job;
    - backup runs in progress are marked interrupted (the restored one succeeded).
    Pending outbox rows are published again with their original IDs (at least once); consumers deduplicate by event ID,
    since the JetStream window is two minutes. A change-feed position beyond the restored feed gets a `gap`.
- **Alternatives:** a single encrypted archive per backup (simpler upload, but no per-file verification or partial
  checks); plaintext local backups as in Hallpass (central's payload files are not encrypted at rest, so that would put
  terminal output and transcripts on backup disks in the clear); a passphrase-derived backup key (a second secret to
  lose; the KEK must be kept anyway to read the restored credentials); resetting subscriptions to "new" (would drop events
  that arrived after the backup) or disabling them for review (stalls every integration after a restore); keeping
  approved approvals (risks a second execution).
- **Consequences:**
  - Losing the KEK loses every backup; key recovery is documented separately (`site/operators/backup.md`).
  - A restore replays event history since each subscription's oldest stored delivery (bounded by delivery retention) and
    may republish outbox events; downstream consumers see duplicates, never gaps.
  - Nodes, users and tokens created after the backup are unknown after a restore; revocations after it are lost.
  - S3 objects are single PUTs (5 GiB each). The S3 target is tested only against the in-process fake (`s3test`).

### ADR-0032 — Human-action follow-up: policies, response routing and one deadline timer (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-07).
- **Context:** SM-HUM-009 asks that a response wakes a sleeping requester per policy, notifies a busy one, survives
  session loss, goes to control or a successor when the requester is unrecoverable, keeps provenance and never reaches
  unrelated sessions. SM-HUM-011 adds rate limits, reminders and escalation, with no model turns spent polling. Until
  now a notification was addressed to the requester's run only (stuck forever once that run ended), every run of a
  space could read every response through `spaceman_human_get`, expiry ran on a once-a-minute ticker, and only pending
  limits existed. This changes who can read a response (narrower for unrelated runs, wider for the rerouted target), so
  it is recorded as a **security-relevant change**.
- **Decision:**
  - **Policies** (`human_policies`, migration `0024`) per action type (`all`, `question`, `approval`, `todo`,
    `message`, `custom`), as installation defaults (no space) or space overrides. The most specific row applies as a
    whole (space+type, space+all, default+type, default+all), then a built-in policy per type (reminders for questions,
    approvals, to-dos and custom types; escalation to high for questions and approvals; none for messages). Fields:
    reminder delay, repeat and count (≤ 20); escalation delay, priority (high/urgent, never lowers) and assignee (a
    user); `requester_ended` `reroute` or `next_launch` with a hold (≤ 7 days); successor role; `wake_idle`; rate limits
    per run and per space per hour (bounded 1–1,000 and 1–10,000). Changing a policy recomputes the schedule of pending
    actions in scope; the computation is deterministic, so central also recomputes it at start.
  - **Routing.** A notification carries a target kind: `run` (the requester's live run, a live run of its agent
    instance, or a live successor-role run, chosen when routed), `instance` (the next run of the requester's instance,
    held until the hold ends), `control` (whichever control run of the space reads it; at most one exists, SM-INV-006,
    and it counts while the supervisor restarts it) or `operators` (undeliverable). A requester is unrecoverable when its
    run exited, failed, was lost, is unknown and not wanted (after a restore) or does not exist. Order on an ended
    requester: `next_launch` → instance (or its live run); otherwise successor-role run → control → operators. A run that
    ends before acknowledging a routed response moves it on (`run_ended`); after five routings it goes to the operators.
    Every routing records the original run, the previous target, the reason (`requester_ended`, `run_ended`,
    `hold_expired`, `operator`, `too_many_routes`) and the time; it is audited (`human_action.rerouted`), published
    (`human.action.rerouted`, metadata only) and, for `operators`, a message to the space's operators (no response text)
    and **Send to control** in the UI (`POST …/notifications/{id}/route`, `human_action:respond`).
  - **No unrelated sessions.** Delivery (terminal notice, `spaceman_messages`, acknowledgement) matches the target
    exactly; there is no role broadcast. `spaceman_human_get` and `spaceman_human_list` hide the response, comments and
    status reason from runs that are not the requester's run or instance, a run the response was routed to, or control.
  - **Wake.** "Sleeping" is an idle program: the existing idle-gated terminal notice wakes it (`wake_idle`, default on);
    a busy run gets it when it turns idle. SpaceMan does not relaunch an ended run to deliver a response: workers are
    started only by control (SM-CTRL-001), and resuming a session outside its owner's decision would bypass that. The
    honest policy values are `reroute` and `next_launch` (a resumed control, a sleeping KB session or an operator
    relaunching the instance picks it up).
  - **One timer.** `human.Service.Run` sets one timer to the earliest expiry, reminder, escalation or hold (indexed
    `min()` queries) and to the earliest approval expiry (an extra deadline), capped at an hour, and is kicked after
    every commit that may move a deadline, after a run ends and when a run turns idle. Its first round routes
    notifications stranded while central was down. The once-a-minute expiry ticker is removed.
  - **Reminders** increment a count and time, schedule the next one, record a change-feed update, an audit event and
    `human.action.reminded`; they do not bump the version (a person answering is not interrupted) or move the action.
    **Escalation** raises the priority and/or assigns, bumps the version, keeps the rank (people's ordering is never
    changed silently), and is audited and published (`human.action.escalated`). Escalation targets a user; SpaceMan has
    no assignee groups.
  - **Rate limits** apply to agent requesters per type: per run and per space in a sliding hour, besides the pending
    limits (500 per space, 25 per run, now counted by an indexed query). The `rate_limited` error tells the agent that
    it is notified of responses, and the MCP tool descriptions say not to wait or check back.
- **Alternatives:** broadcasting a rerouted response to every run of a role (more sessions see it, against SM-HUM-009);
  relaunching or resuming the requester's session to deliver it (bypasses control; an unsupervised launch); a ticker per
  job or per space (polling); merging policies field by field (harder to reason about which policy applied).
- **Consequences:**
  - A response for an ended worker reaches control, which can read it; a configured successor role's run can too.
  - Pending actions created before `0024` get reminder and escalation times from the policies at the first start.
  - Built-in reminders and escalation apply to existing installations without configuration; set an `all` default
    with zero delays to turn them off.
  - Real-harness evidence for waking and routing is the simulated suite plus the dev stack (stub harness); no real
    Claude Code or Codex run was exercised for this package.

### ADR-0033 — Revocation reaches open streams at once, with a bounded periodic backstop (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-01, SM-AUTH-007).
- **Context:** SM-AUTH-007 asks that revocation applies to requests and to active streams under a documented bounded
  policy. Every request is authenticated and authorized when it arrives, so requests were already covered. Long-lived
  connections were not: the change feed (`GET /watch`) and the live terminal (`GET …/runs/{run}/terminal`) re-checked
  their principal only every `auth.stream_revalidate` (default 30 s, accepted up to 5 min), and a node connection
  (`/agent/v1/connect`) was checked only when it connected. The policy defines how long revoked access may continue,
  so it is recorded as a **security-relevant change**.
- **Inventory (before → after):**

  | Credential or connection | Revoked by | Before | After |
  |---|---|---|---|
  | Session cookie (password, break-glass) | logout, revoking one or all sessions, rotation (step-up, MFA), password change or reset, MFA enrollment or removal, grant change, user disabled, identity unlinked | next request refused; open streams up to 30 s (5 min max) | next request refused; open streams end at once |
  | SSO session (OIDC) | as above; SSO grants change at sign-in | as above | as above |
  | API token | revoked (with the tokens it issued), creator disabled, creator's grants reduced | as above | as above (grant reduction: per-item filtering or `forbidden`, below) |
  | Root token | rotated, revoked | as above | as above |
  | Session or token expiry, session idle timeout | time | up to 30 s | up to `auth.stream_revalidate` (default 30 s, now at most 60 s) |
  | Grant removed or reduced (space access) | user grants, SSO grants, token deleted | change feed filtered per item at once; terminal up to 30 s | change feed filtered per item at once (stream stays open while its credential is valid); terminal ends at once with `forbidden` |
  | Node credential | node disabled or revoked, re-enrollment | connection closed at once | unchanged (closed at once) |
  | Node credential expiry, revocation outside central | time, database change | connection kept until it dropped | connection closed within `auth.stream_revalidate` |
  | Run credential | run stopped, ended, failed, relaunched; restore | every tool call authenticates it | unchanged (next tool call fails) |
  | Downloads (recording, transcript, attachment) | — | authorized when the request starts | unchanged: bounded by the file, not re-checked mid-transfer |
- **Decision:**
  - **Event-driven revocation.** `auth.Revocations` is a small in-process notifier. Every transaction that revokes a
    session, all of a user's sessions, an API token or the root token, or changes grants, registers a signal with
    `tx.AfterCommit` (never inside the transaction; a rollback signals nothing; the authorization engine reloads before
    the grant signal). Each open stream holds an `auth.StreamWatch` keyed by its user, session (token hash and display
    ID) and token ID. A signal wakes the matching watches (a grant change wakes all), and the stream re-authorizes: it
    resolves its principal again without marking the session active and applies its own permission check. A watch has
    a one-slot channel, so bursts coalesce and no signal is lost. A revocation generation counter closes the race
    between authenticating a request and registering its watch.
  - **Periodic backstop.** Every stream also re-authorizes every `auth.stream_revalidate` (1 s to 60 s, default 30 s;
    the maximum was 5 min). This covers expiry, idle timeout and anything changed outside this process (the offline
    CLI). Node connections re-check their credential and node state on the same interval.
  - **Stream endings.** A stream closed for access reasons ends with `event: end` and `{"reason":"unauthenticated"}`
    (the credential is no longer valid) or `{"reason":"forbidden"}` (still valid, but no longer grants
    `terminal:watch` in the space). The change feed never ends as `forbidden`: it filters each change by the current
    grants, so a space whose grant was removed disappears from the next change on. The web UI re-checks its session on
    `unauthenticated`: a session that is gone goes to sign-in; a session rotated by a step-up reconnects where it left
    off. A terminal that ends as `forbidden` shows "Access lost".
  - **Cost.** A re-check is one principal lookup on the read pool (session or token row, user, grants: about four
    indexed reads; tokens may record their last use at most once a minute) plus an in-memory Casbin check. Each stream
    re-checks once per interval and once per signal that matches it; a grant change re-checks every open stream once.
    No stream polls the database more often than that. Node connections cost two indexed reads per interval.
  - **Revocation never undoes completed actions.** Revoking a credential refuses its next request and ends its
    streams. Operations already committed with it stay committed and audited under it: users it created, runs it
    launched (which keep running), approvals it decided (each use still rechecks the approver, SM-APPR-003), messages,
    tasks and events. Stopping work is a separate, explicit action (stop the run, pause the space, disable the node).
- **Alternatives:** periodic re-checks only, as Hallpass's change feed does (the bound would be the interval for every
  revocation); a shorter interval (more database reads for every open tab, and still not immediate); closing streams
  from the eventbus topics (the bus may drop events for a slow subscriber, and it carried no session revocations);
  per-stream database polling at a high frequency (cost grows with open tabs).
- **Consequences:**
  - Revocations made by this central end affected streams within milliseconds (tests assert under 5 s with the
    periodic re-check at 60 s). Expiry and out-of-process changes are bounded by `auth.stream_revalidate`; a node
    connection's bound is the same.
  - A grant change wakes every stream for one re-check; grant changes are rare administrative actions.
  - Configurations with `auth.stream_revalidate` above 60 s no longer validate.
  - Not covered: a download already in progress continues to its end; data a viewer already received is not taken
    back.

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
| K-11 | ~~Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26).~~ **Resolved 2026-11-02:** SSO grants are stored under `user:<id>#sso`, which only SSO sessions act as (ADR-0029); SR-26 ported as `TestReviewSR26SSOGrantsOnlyForSSOSessions` (`internal/auth`, `internal/oidc`) and `TestOIDCSignInOverHTTP`. | — | done |
| K-10 | ~~Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode.~~ **Resolved 2026-11-02:** works interactively with an empty config dir once first-run state and folder trust are seeded (spike S-5, ADR-0021). | — | done |
