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
    `spaceman-agent lease-guard --lease-file <space run dir>/lease.json`. It ends with the space's last live run (ADR-0043).
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
  password sign-in off, so the guards have nothing to protect yet. *All three were ported later in ADR-0045.*
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

### ADR-0034 — Packaging: what the images contain, and a looser sandbox for the node agent (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-09).
- **Context:** SM-PKG-001 asks for binaries, the embedded UI, systemd units, container images, example configuration,
  deployment and upgrade docs, release automation, checksums, notices and SBOMs. SM-PKG-002 asks the docs to state
  which images include Git, herdr-core and harnesses, and forbids advertising an image as agent-ready without its runtime
  dependencies. SM-PKG-003 limits the production target to Linux. The packaging is ported from go-hallpass v0.7.0
  (`bdd8329`, Apache-2.0, same author): `Dockerfile`, `deploy/systemd/hallpass.service`, `deploy/compose.yaml`,
  `scripts/release-build.sh`, `scripts/container-smoke.sh` and `.github/workflows/release.yaml`. Two facts force
  departures from it. First, central runs `git` (`internal/memory` for role memory, `internal/backup` for memory
  bundles), so Hallpass's distroless base is not enough. Second, the node agent starts herdr-core and harnesses that run
  arbitrary developer tooling, which Hallpass's sandbox would break. Relaxing a sandbox and choosing a base image with a
  shell are recorded as a **security-relevant change**.
- **Decision:**
  - **Central image** (`Dockerfile`): Debian 13 slim, pinned by digest, with `git`, `ca-certificates` and `tini` as
    PID 1 (it reaps orphaned git helpers). It runs as non-root 65532, with the UI embedded (`-tags embedui`), and is
    built with `-trimpath`, an empty build ID and buildinfo ldflags. It contains no herdr-core, no harness and no
    `spaceman-agent`. Distroless static was rejected because it has no git. Without git, central would run with role
    memory disabled and refuse backups that contain memory, and an image should not ship a degraded central by default.
    The cost is a shell and the Debian package database in the image. Package versions are not pinned (no
    snapshot.debian.org), so rebuilds are not byte-identical.
  - **Node agent base image** (`Dockerfile.agent`): Debian 13 slim (glibc, so binaries the operator adds can run) with
    `spaceman-agent`, `git`, `openssh-client`, `ca-certificates` and `tini`. It runs as 65532 with
    `HOME=/var/lib/spaceman-agent`. Its labels (`io.spaceman.agent-ready=false`, `contains.herdr-core=false`,
    `contains.harnesses=false`) and the docs say it is **not agent-ready**, and `run` fails at startup without `hc`.
    Nothing is bundled whose redistribution terms are unverified:
    - **herdr-core.** `docs/contracts/herdr-core.md` and `docs/THIRD_PARTY.md` record Apache-2.0 from inspecting an
      installed binary, without a source checkout or an upstream licence file. No release download location, checksum
      source or redistribution terms have been verified.
    - **Claude Code and Codex.** These are third-party products whose licence terms this project has not reviewed for
      redistribution.

    The extension path is `deploy/examples/agent-runtime/Dockerfile`, which the operator builds from binaries they
    obtained themselves:
    - it is `FROM` the base image;
    - every file in its `bin/` must be listed in a `SHA256SUMS` the operator provides, and `hc` is required;
    - harnesses that need a language runtime get a marked section for the vendor's documented install method;
    - its labels say `operator-supplied` and `operator-assembled`.

    SpaceMan names no download URLs.
  - **Central unit** (`deploy/systemd/spaceman.service`): Hallpass's hardening, unchanged in substance. That means no
    capabilities, `NoNewPrivileges`, `ProtectSystem=strict` with only the `StateDirectory` writable, `ProtectHome`,
    private `/tmp`, `/dev` and IPC, `ProtectProc=invisible`, `ProcSubset=pid`, `RestrictNamespaces`,
    `MemoryDenyWriteExecute`, and `@system-service` minus `@privileged @mount … @debug …`. Its
    `AssertPathExists=/etc/spaceman/kek` matches the KEK rules (mode `0400`, owned by the service user, outside the data
    directory).
  - **Agent unit** (`deploy/systemd/spaceman-agent.service`). It keeps no capabilities, `NoNewPrivileges` (no `sudo` or
    setuid from agent sessions), `RestrictSUIDSGID`, `ProtectSystem=strict` with only the data directory writable,
    `ProtectHome`, private `/tmp` and IPC, kernel protections, `ProtectProc=invisible` and `@system-service`. These are
    the relaxations (marked RELAXED in the unit):
    - **`MemoryDenyWriteExecute` is not set.** JIT runtimes used by harnesses and tools need writable and executable
      memory.
    - **`RestrictNamespaces` is not set.** Harness and tool sandboxes may create namespaces. Which ones do is untested,
      and the unit tells operators to set it back when their tooling works without.
    - **`ProcSubset=pid` is not set.** Runtimes read `/proc/cpuinfo` and `/proc/meminfo`.
    - **`@debug` is not denied.** Debuggers, profilers and sanitizers use `ptrace` and `perf_event_open`.
    - **PTY devices are allowed.** `DeviceAllow=/dev/ptmx` and `char-pts` are explicit, because herdr-core needs pseudo
      terminals.

    `HOME` is the data directory (git and ssh configuration for SM-REPO-005). `RuntimeDirectory=spaceman-agent` holds
    the hc and bridge sockets. `KillMode=control-group` means stopping the unit stops the agent, its herdr-core core
    and every run on the node. The alternative was `KillMode=process` with `RuntimeDirectoryPreserve=yes`, which would
    let runs survive an agent restart and be adopted, bounded by the lease guard (ADR-0017). It was not chosen as the
    default: it is untested under systemd, `systemctl stop` would leave agents running until their lease file expires,
    and systemd discourages it. The unit documents it as an untested option.
  - **Release archives** (`scripts/release-build.sh`):
    - `linux/amd64` and `linux/arm64` by default. A `darwin/*` build happens only when named, and is labelled
      `_unsupported` in the file name with an `UNSUPPORTED-PLATFORM.txt` inside (SM-PKG-003).
    - `CGO_ENABLED=0`, `-trimpath`, `-buildid=`, buildinfo ldflags, `SOURCE_DATE_EPOCH` from the commit, the UI
      embedded, and GNU tar with sorted, numeric-owner and fixed-mtime entries for byte-reproducible archives.
    - Each archive holds both binaries, the units, the example configs, LICENSE, NOTICE, THIRD_PARTY.md and
      CHANGELOG.md.
    - SPDX SBOMs come from `syft` when it is installed: one per archive and one for the source tree, including the web
      UI's locked npm packages. `SBOM=auto` skips them with a printed notice when syft is missing; `require` fails.
    - `SHA256SUMS` covers the archives and SBOMs.
  - **CI** (`.github/workflows/release.yaml`, actions pinned by SHA like `ci.yaml`): runs on `v*` tags and manual
    dispatch. The steps are:
    1. a vet and race gate, then the UI build;
    2. archives, SBOMs (`SBOM=require`, syft `v1.54.0`) and checksums;
    3. both images for linux/amd64, built with `push: false` and run through `scripts/container-smoke.sh`;
    4. for a tag only, a **draft** GitHub release with the archives, SBOMs and `SHA256SUMS`. A maintainer publishes it
       by hand.

    No image is pushed to any registry, and the release notes say images are not published.
- **Alternatives:**
  - A distroless central with git copied in. A dynamically linked git plus its helpers is fragile without a package
    manager.
  - Chainguard or Wolfi git images. These add a second distribution and its licence review.
  - Bundling herdr-core and the harnesses. Redistribution terms are unverified.
  - An agent image that downloads them at build time from built-in URLs. That would invent upstream facts and hide a
    supply-chain step from the operator.
  - Reusing the central sandbox for the agent. It is safer on paper, but JIT runtimes and pseudo terminals break.
  - Publishing images from CI. Out of scope until a registry and its credentials are decided.
- **Consequences:**
  - Neither image is agent-ready. A containerized node needs an operator-built runtime image, and that combination
    (herdr-core and harnesses inside a container) is untested.
  - The agent's user can do more than central's: run JIT code, create namespaces and trace its own processes. The trust
    model is unchanged, since a space was never an isolation boundary (SM-INV-007, SM-SEC-001). The sandbox still
    protects the rest of the host: no privilege gain, a read-only OS and hidden `/home`.
  - `systemctl restart spaceman-agent` ends runs, so node upgrades need a pause or a quiet period
    (`site/operators/upgrade.md`).
  - The units passed `systemd-analyze verify` on systemd 257 in a Debian 13 container, but have not run a real
    installation on a Linux host. Release CI has not run yet.

### ADR-0035 — Terraform provider: nested module, write-only secrets, If-Match from private state, no silent replacement
- **Status:** Accepted, 2026-11-03 (M5-01 phase 1).
- **Context:** SM-API-004 asks Terraform to manage the durable configuration (spaces, roles, catalogs, account references,
  MCP and skills, events, human-action types and policies, KB, limits, schedules, cross-space and auth configuration),
  never runtime state; SM-API-005 asks for normal drift from UI edits, ownership of only the represented fields, safe
  relocation on a `node_id` change, and no perpetual plans from secrets or immutable publication. The API (ADR-0003) already
  has optimistic concurrency, idempotency keys and RFC 9457 problems; several catalog objects have no delete, and
  relocation (M4-05), limits and schedules (M4-02) and cross-space requests (M4-06) are not built.
- **Decision:**
  - **Layout.** *(Superseded by ADR-0047: the provider now lives in its own repository.)* A nested Go module
    `terraform-provider-spaceman/` in this repository (no separate repository) on HashiCorp
    terraform-plugin-framework v1.19.0 (protocol 6), framework-validators v0.19.0 and terraform-plugin-testing v1.16.0.
    It has its own small typed client over the API's JSON (`internal/client`) and imports nothing from the central
    module (whose packages are `internal/` anyway). Root `make check` runs its unit tests (`tf-test`); `make tf-acc` runs
    the acceptance tests.
  - **Authentication.** `Authorization: Bearer` with an API token or the root token (`SPACEMAN_ENDPOINT`,
    `SPACEMAN_TOKEN`). Plain `http://` only to loopback hosts unless `allow_insecure_http` is set (opt-in; the token would
    cross the network in clear text). Redirects are never followed, so the token never leaves the configured origin. The
    client logs nothing, and errors carry the problem's code, detail and field errors, never bodies.
  - **Concurrency and retries.** The server version of every object is kept in Terraform private state (refreshed on
    every read) and sent as `If-Match` on updates; a 409 tells the operator the object changed after the plan. Creates send
    a fresh `Idempotency-Key` where the API accepts one (spaces, accounts, MCP servers, OIDC providers) and only those, and
    GETs, are retried on transport errors and 502/503/504; other creates are never retried, so a lost response can never
    create a second object. One tolerated conflict: a subscription that follows a template's latest version is moved by
    the publication of that template in the same apply; the update is retried once when that move is the only change.
  - **Ownership (SM-API-005).** Optional fields are Optional+Computed with the prior value kept while unknown: a field the
    configuration sets is owned and UI edits to it are drift; a field it leaves out keeps whatever SpaceMan holds. Removing
    a field from the configuration therefore leaves the last value in place (documented). JSON attributes (schemas, UI
    metadata, template definitions) compare semantically, so formatting never causes a diff; subscription variables
    compare only the configured keys (defaults filled in by SpaceMan are not drift). `spaceman_space_accounts` owns a
    space's whole binding set because the API replaces it as a whole.
  - **Secrets.** Account secrets, MCP request headers, Pulse `.creds` and OIDC client secrets are Terraform write-only
    arguments (`<name>_wo`, Terraform ≥ 1.11), never in plan or state, sent on create and whenever the companion
    `<name>_wo_version` changes. The API's own counters (`secret_version`, `headers_version`, `client_id`,
    `has_client_secret`) make rotations visible without revealing values.
  - **Immutable versions.** Prompts, skills, event templates and human-action types are resources whose content describes
    the latest published version. A content change saves the draft and publishes it (one version per change); an unchanged
    configuration never publishes. A foreign unpublished draft is not drift; publishing replaces it with a warning (event
    templates have no draft discard, so refusing would deadlock).
  - **No silent replacement.** Where the API cannot change a field in place and the object cannot be deleted or holds
    history (space slug, catalog slugs, kinds, harness, a prompt's name and description, KB and dream slugs), a plan-time
    error refuses the change instead of `RequiresReplace`. A `node_id` change is refused at plan time with a diagnostic
    naming the missing relocation (SM-SPACE-005…008, M4-05); it is never a destroy and recreate (SM-INV-003). Objects
    that are cheap and safe to recreate (grants, mappings, approval policies, an account's kind, an MCP server's slug)
    use ordinary replacement.
  - **Destroy.** Objects without a delete API (prompts, skills, event templates, human-action types, harness profiles,
    role templates) leave Terraform's state with a warning; knowledge bases and dream configurations are disabled;
    spaces are archived, and purged only with `purge_on_destroy = true`.
  - **Scope of phase 1.** Resources for everything the API supports today (20) and data sources for nodes, spaces and
    event templates. Not managed: runtime state, nodes (one-time enrollment secret), API tokens (secret shown once).
    Phase 2: limits, schedules, cross-space configuration and relocation (after M4-02/05/06), space repositories (no API),
    backup target and schedule (configuration file only), users and role grants.
  - **Docs.** `tfplugindocs` is not available offline in this environment, so `docs/` (registry layout) is rendered from
    the schemas and `examples/` by `internal/provider/docs_test.go`, which also fails when the docs are stale.
- **Alternatives:** a separate repository (the plan keeps the provider next to the API it tracks); generating the client
  from `api/openapi.json` (large generated surface for ~60 operations, and the provider would still need hand-written
  semantics); the plugin SDK v2 (no write-only arguments or private-state ergonomics of the framework); `RequiresReplace`
  for `node_id` (would destroy and recreate a pinned space, which SM-INV-003 forbids); sensitive-but-stored secret
  attributes (secrets in state); keeping the object version as a computed attribute (UI edits would show as "known after
  apply" noise on every plan).
- **Consequences:**
  - Terraform ≥ 1.11 is required. Write-only values must be fed from ephemeral variables or resources to stay out of plans.
  - A value removed from the configuration is not reset; operators must set it explicitly to change it.
  - Catalog objects destroyed by Terraform remain in SpaceMan; re-creating one with the same slug fails until it is imported.
  - When relocation lands, `node_id` changes become in-place updates; the plan-time refusal is the only part to replace
    (done in ADR-0037).
  - No central API changes were needed. The provider is not published to a registry.

### ADR-0036 — One limits model, space schedules on the shared deadline timer, and replays recorded apart from retries
- **Status:** Accepted, 2026-11-03 (M4-02).
- **Context:** SM-LIMIT-001…003 and SM-EVT-015. Limits existed as scattered constants and per-feature settings: the
  queue's active limit (queue policy, default 1), each knowledge base's `max_sessions`, human pending limits (500 per
  space, 25 per run, constants), human rate limits (human-action policies, ADR-0032), the attachment quota
  (`artifacts.space_quota_bytes`), MCP gateway limits per server, the 512 MiB recording quota on the node and dream
  budgets per dream configuration. The node agent's `capacity` was reported in its hello and never enforced, and nothing
  bounded a space's concurrent runs, tool calls per run, ingestion per subscription, task run time or spending. A KB
  support session could ask another knowledge base, whose session could ask back (unbounded support recursion). Replays
  were cursor changes inside a subscription update; the replay boundary lived in memory (lost on restart), duplicates
  during a replay were indistinguishable from stream retries, and no schedule mechanism existed for spaces.
- **Decision:**
  - **One model, one owner per limit.** `internal/limits` defines every limit (key, unit, bounds, zero-means-none,
    enforcement point, what lowering does). Installation defaults come from the new `limits:` configuration section
    (bounds in `config.LimitRanges`, checked at load), the storage quota stays `artifacts.space_quota_bytes`. A space's
    overrides are one versioned row (`space_limits`, migration 0025), replaced as a whole by `PUT /spaces/{id}/limits`
    (If-Match once stored, audited `space_limits.updated` with from/to per changed key). `GET /spaces/{id}/limits` is the
    single effective read: value, default, override, source (`installation`, `space`, `queue_policy`) and the related
    limits owned elsewhere (node capacity, per-KB sessions, per-type human rates, per-server MCP limits, the recording
    quota, dream budgets). The top-level task limit stays in the queue policy (its default now comes from
    `limits.max_top_level_tasks`); subsystems are not rewritten, their existing knobs read the effective value.
  - **Enforcement points.** Run launch (inside the launch transaction, through a new `fleet.Options.Admit` hook):
    worker and diagnostic runs against the space's `max_concurrent_runs` (default 8) and the node's reported capacity
    (now enforced; 0 or unknown means unbounded); budgets for every kind. Human-action creation: pending limits.
    KB scheduler: `max_kb_sessions` per space on top of each KB's `max_sessions`. KB ask tool: `support_depth`.
    Agent tool gateway: `max_tool_calls_per_run`, counted durably per run (`run_tool_calls`; the catalog listing is not a
    call). Event ingestion manager: `ingest_per_minute` per subscription with a token bucket; an event waits at most 10 s
    and is otherwise left in the stream (negative acknowledgement), so nothing is dropped. Attachment upload: the space's
    storage quota. Deadline timer: `task_deadline_minutes`.
  - **Pools and recursion (SM-LIMIT-002).** Control (one per space), KB support (per-KB and per-space pools) and dreamer
    runs never count against the worker concurrency limit or node capacity, and workers never count against the support
    pools, so neither side can starve the other. Support depth defaults to 1: a KB session cannot ask a knowledge base at
    all (`kb_sessions.depth` records each session's depth; a deeper request is refused with `forbidden`), and it never
    started workers (ADR-0026).
  - **Lowering never kills work.** Every limit only refuses new work beyond it. The one policy that stops work is a
    space's explicit `on_lower: stop` for the concurrency limit: lowering it stops (interrupt, then kill after the grace)
    the newest worker and diagnostic runs beyond the new value, audited as stop requests. The default is `drain`.
    A passed task deadline is reported once (audit `task.deadline_passed`, a notification to the operators and to
    control) and never stops, fails or moves the task.
  - **Budgets are usage-based, never a currency guarantee.** `budget_tokens_24h`, `budget_cost_cents_24h` (sum of the
    harnesses' own estimates) and `budget_tokens_per_run`, all off by default. The 24-hour window is rolling over runs
    started in it; unknown usage is counted as unknown, never zero. At `budget_warn_percent` (default 80) operators get
    one notification per budget and window (`budget_notices`); past a space budget, new runs of every kind are refused
    until reported usage is below it or the budget is raised; running runs are never stopped and the per-run budget only
    warns. API, UI and docs say that usage is reported late, so this is not a hard limit (SM-LIMIT-003, SM-REC-005).
  - **Schedules.** `internal/schedule` (`space_schedules`, `schedule_occurrences`): a schedule creates an inbox item
    (new source kind `schedule`; `inbox_items` is rebuilt in 0025 to allow it, data-preserving) or a ready task (origin
    `schedule`) at cron or interval occurrences. Cron expressions are five-field or descriptors, parsed with
    `github.com/robfig/cron/v3` v3.0.1 (MIT, widely used, parser only; `@every` and `TZ=` prefixes refused) in an IANA
    time zone (`time/tzdata` embedded, so containers need no zone database); intervals are anchor + n × interval
    (60 s to 366 days). Each occurrence is recorded once (primary key schedule + time) and its key
    `schedule:<id>:<unix>` is the deduplication key of the item or task, so a crash between creating and recording never
    duplicates. An occurrence handled more than a minute late was missed: `skip` records missed occurrences (one summary
    row), `run_once` runs the latest once, `catch_up` runs up to `catch_up_max` (1-100) of the latest, oldest first;
    late items say so. `skip_if_open` (default on) skips an occurrence while what the previous one created is open.
    Archived spaces are not scheduled. One timer drives everything: schedules and task deadlines are jobs of central's
    single deadline timer (ADR-0032), woken after commits; nothing polls per schedule or space, and agents are never
    woken to wait for a schedule.
  - **Replay (SM-EVT-015).** `POST /spaces/{id}/subscriptions/{sub}/replay` (replay:execute, If-Match, a required
    reason, a position `all|time|seq`, optional `fresh_items`) is the explicit operation; it bumps the cursor generation,
    records a `subscription_replays` row and audits `event_subscription.replayed` with the reason. A cursor change
    through an update that re-delivers history is recorded the same way. The ingestion manager persists the replay
    boundary when it recreates the consumer, so events stored before it count as replayed across restarts. Every
    delivery records its origin (`live`, `redelivery` — the stream retrying after a nak, an acknowledgement timeout, a
    restart or a restore —, `replay`, `reprocess`), its attempt (deliveries of that event so far) and the replay ID;
    replayed items carry `delivery_origin` and `replay_id` in their trusted metadata. Inbox deduplication holds during a
    replay (an event that made an item is recorded as a duplicate replay); `fresh_items` adds the replay ID to the key,
    so each replay may create one new item per event.
- **Alternatives:** a generic key-value limits table per subsystem (no single effective read, sources unclear); moving the
  queue policy's limit into the new model (two write paths, or an API break for an existing object); counting KB and
  control runs against the worker limit with reserved slots (couples the pools, starvation still possible under a low
  limit); stopping runs on budget exhaustion (claims a precision usage reports do not have); a per-schedule goroutine or
  ticker (polling, SM-ARCH-004); writing a cron parser (error-prone; robfig's parser is small and stable); recording
  redeliveries as a new outcome value (would need another table rebuild and mixes "what happened" with "why").
- **Consequences:**
  - New launches can now be refused with `rate_limited` for concurrency, node capacity or budgets; KB sessions can no
    longer ask knowledge bases unless a space raises `support_depth`.
  - Each agent tool call is one more small write transaction (the per-run counter).
  - Throttled ingestion may make JetStream redeliver prefetched events after its acknowledgement timeout; they are
    deduplicated and recorded as redeliveries.
  - Terraform phase 2 (ADR-0035) can manage `space_limits` and schedules through these endpoints; not done here
    (done in ADR-0041).
  - Merging note: migration 0025 rebuilds `inbox_items`; later migrations that rebuild it must keep `schedule`.

### ADR-0037 — Explicit, resumable space relocation with clean-state checks and a fenced switch-over (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-05).
- **Context:** SM-INV-003 allows only explicit relocation of a stopped space (UI, API, Terraform). SM-SPACE-005 asks for
  blocked launches, stopped runs, synced memory, code pushed or explicitly exported, uploaded artifacts, detection of
  dirty, untracked and unpushed state, no silent loss, a prepared destination, an atomic switch, a stopped space and a
  retained source folder; SM-SPACE-006 separates desired destination and effective assignment; SM-SPACE-007 makes
  unavailable-source recovery explicit with an acknowledgement; SM-SPACE-008 guarantees project continuity and labels
  native session resumption; SM-API-005 asks Terraform to relocate on a `node_id` change. Relocation moves execution
  authority between machines, accepts node-produced files on central, lets the node run Git in agent-writable
  repositories, and adds node commands that run without a lease, so it is recorded as a **security-relevant change**.
- **Decision:**
  - **State machine on central.** Table `relocations` (migration `0026`), one in progress per space (unique partial index,
    prechecks included). Modes `clean`, `source_unavailable` and `precheck`. States `draining → checking → [blocked] →
    [exporting] → preparing_destination → finalizing → done`, with `checked` (precheck), `failed` and `cancelled`.
    `spaces.desired_node_id` holds the destination while `spaces.node_id` stays the effective assignment; launches,
    resume and archive are refused and `syncLeases` revokes the source's lease. Every transition is one transaction that
    checks the relocation's version; I/O (memory bundles, digest checks) happens before it. Steps resume after a central
    restart: when a node connects, when a run ends, and at start (`RunRelocations`). Nothing creates a relocation except
    `StartRelocation` (API `POST /spaces/{id}/relocation`, `space:relocate` with step-up for sessions).
  - **Protocol minor 3** (by negotiation: both nodes must have last negotiated ≥ 3). Commands
    `space.relocate.check|export|prepare|release` are durable `node_commands` like every other command, carry the space's
    folder name, and are accepted by the node **without a lease** (they are not execution; the source's lease is revoked
    and the destination has none). A repeat re-sends the stored outcome. Outcomes travel as `relocation.report` (state:
    the node re-sends the last week's on reconnect; central applies only the one for the step it waits for, from the node
    it expects). Exported files go up as `relocation.upload` chunks (stop-and-wait, resumable, 1 MiB, SHA-256 checked,
    4 GiB per relocation) to `<data>/relocations/<id>/`; the destination pulls them and central's memory bundles with
    `relocation.fetch`/`relocation.data`, only while preparing and only as the destination.
  - **Check.** The source checkpoints and syncs the space's role memory, re-ships recordings, resumes transcript uploads and
    waits (60 s by default) for acknowledgements, then inspects the space folder without following symlinks: each
    repository (remotes with credentials removed, branches with commits not on their upstream or, without one, on no
    remote-tracking ref; changed, untracked, stashes, detached commits) and its worktrees, loose files outside
    repositories, and instance folders (reported, not carried). Git runs with the node's environment (its own
    credentials, SM-REPO-005) but with hooks and fsmonitor disabled. Unpushed is judged against the remote-tracking refs
    the node has (no fetch).
  - **Policy.** Unsynced memory, pending recordings or transcripts, active runs and inspection errors block (retry). Local-
    only Git state and loose files are refused by default (`dirty_state = refuse`, audited `relocation.refused`, outcome
    denied) and carried only by an explicit export.
  - **Export format.** Per repository a Git bundle of every ref (`repo-<n>.bundle`) plus temporary refs under
    `refs/spaceman-relocation/` for snapshot commits of the main work tree and each worktree (built in a temporary index:
    tracked changes, deletions and untracked non-ignored files on top of HEAD), detached HEADs and every stash entry; the
    refs are removed again. Loose files are a `loose.tar.gz` (regular files and symlinks). The manifest (branches,
    upstreams, remotes, worktrees, stashes, snapshots, blob digests) is stored with the relocation. The destination
    restores each repository from its bundle (branches, tags, remote-tracking refs, notes), re-adds remotes and upstreams,
    checks out the branch, applies the snapshot with `restore --worktree` (changes come back unstaged), re-stores stashes
    oldest first, re-adds worktrees, and unpacks loose files refusing traversal, overwrites and writes through symlinks
    (symlinks are created last).
  - **Prepare and verify.** The destination moves an existing folder of the space aside (`retained/…-previous-…`, never
    deleted; its own unfinished attempt is restarted), clones pushed repositories at the source's commit with their local
    branches and worktrees, restores role memory from central's acknowledged history (bundles of `main` at the recorded
    head; the node journal records it as acknowledged so later syncs fast-forward), and reports. Central checks every
    repository commit and memory head against what it sent.
  - **Finalize.** One transaction sets `node_id` to the destination, clears `desired_node_id`, moves the lease to the
    destination at generation `max(previous) + 1` (expired, so it is granted at the destination's next sync), cancels
    commands still queued for the source, revokes the space's run credentials, marks the source's runs lost in the
    unavailable flow, queues the source's release and audits `space.relocated`. The space stays paused. Central now fences
    the old node: run reports for a space assigned elsewhere are ignored and a still-running run gets a kill; memory syncs
    are refused (ownership); a node's claimed leases are checked when it connects, before commands are delivered.
  - **Source folder.** `recovery = retain` (default) moves the folder and the space's memory to
    `<data>/retained/<workdir>-<id>/` with a note; `retain_days` > 0 schedules deletion (one timer for the earliest due
    retention); `0` keeps it until `POST …/relocation/release`. `delete` removes it. Exported files on central are kept
    until the source copy is deleted.
  - **Unavailable source.** Allowed only while the source is not connected (offline, disabled or revoked). Requires
    `acknowledge_loss` and an acknowledgement text (10–500 characters), audited as `relocation.loss_acknowledged` (text
    sealed as a sensitive detail, also shown on the relocation). The loss report lists memory not acknowledged at the last
    node report, active runs, the last inspection and the kinds of state central cannot see. The destination is prepared
    from central's memory and operator-listed repositories (credential-free URLs) only.
  - **Session continuity.** Project continuity (repositories, memory, task and communication state) is guaranteed. Native
    sessions are node-local (`docs/contracts/harnesses.md`) and not carried: every relocation says "Fresh session, project
    continuity only", and a launch that asks to resume a session that only ever ran on another node starts a fresh one,
    recorded in the run manifest. No harness capability was added.
  - **Terraform.** A `node_id` change is an in-place update that starts (or resumes, retrying a blocked one with the
    configured policy) a clean relocation and waits (`relocation_dirty_state` default `refuse`, `relocation_stop_runs`,
    `relocation_recovery`, `relocation_retain_days`, `relocation_timeout`); a refusal fails the apply with the findings.
    The unavailable-source flow is not offered through Terraform. Replaces ADR-0035's plan-time refusal.
- **Alternatives:** moving the folder through central as one archive (copies pushed state too and makes central a relay for
  whole repositories); fetching on the destination from the source directly (nodes would need to reach and authenticate
  each other); auto-committing dirty work to a branch and pushing it (violates SM-REPO-004/006); carrying native session
  files (harness formats are not a contract and embed node paths); requiring a lease for relocation commands (the source
  must lose it before the check, or new work could start).
- **Consequences:**
  - A relocation with dirty state needs an operator decision; Terraform applies fail until it is made.
  - Exported state and memory bundles sit on central's disk in plaintext while the relocation needs them (like central's
    other payload files).
  - A source that never returns keeps `source_state = pending`; its folder is untouched until it reconnects.
  - Staged-versus-unstaged distinctions and ignored files are not carried; worktrees outside the space folder and linked
    worktrees of outside repositories block the check.

### ADR-0038 — Cross-space requests: two-sided grants, destination-owned execution, disclosure on the reply (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-06).
- **Context:** SM-XSPACE-001…003, SM-INV-008 and SM-SPACE-001 ask for collaboration between spaces through authorized
  SpaceMan operations only: separate discovery, question, delegation, disclosure and knowledge permissions; the
  destination's control accepts or declines and owns its execution, with no remote worker creation; correlation,
  deadlines, cancellation, depth, quotas and loop protection; KB access only through explicit authorized requests. Until
  now no data crossed a space boundary at all. This opens a channel between spaces, so it is recorded as a
  **security-relevant change**.
- **Decision:**
  - **Grants per ordered pair, on both sides** (`xspace_peers`, migration `0027`). A space configures, per peer space,
    what it accepts (inbound: `discover`, `questions`, `delegation`, `knowledge` with an optional KB list, `disclosure`,
    `max_per_hour` 1–1,000, `max_pending` 1–100) and what its own control may send (outbound: `questions`,
    `delegation`, `knowledge`). Every flag is off unless set; a missing row allows nothing. A request of kind *k* from S
    to D needs D's inbound *k* **and** S's outbound *k*. The brief's "optionally the source" is tightened to "the source
    must also allow it": a request carries the source's information out of the source, so the source's operators decide
    too. Each space edits only its own row (`xspace:write` in that space, held by operators); there is no way to grant
    another space's side. Changes are versioned, audited (`xspace.peer_created/updated/deleted`) and in the change feed.
  - **Discovery is separate and not needed to ask.** With `discover`, the peer's control lists the space with its profile
    (`xspace_settings.profile`), what it may send, the KBs it may ask, the disclosure level and the limits. Without
    `discover` and without the requested grant, a request answers `not_found` exactly as for a space that does not
    exist, so existence is not disclosed; with `discover` but not the grant it answers `forbidden`.
  - **Only control asks or answers.** The eight tools (`spaceman_xspace_discover`, `_request`, `_get`, `_list`,
    `_accept`, `_decline`, `_reply`, `_cancel`) are registered for the control kind only and the service refuses any
    other kind as well. Control is the space's one accountable identity (SM-INV-006, SM-CTRL-001 "handles … cross-space
    requests"); letting workers, KB sessions or dreamers speak for a space would let any prompt-injected run move data
    out of it. Operators can withdraw outgoing and decline incoming requests through the API and UI; they do not answer
    for control.
  - **Destination-owned execution.** A question or delegation is stored together with an inbox item in the destination
    (source kind `cross_space`, the source's text marked as untrusted content) in one transaction; the supervisor's
    existing inbox notification wakes the destination's control through the idle-gated notice. Its control accepts
    (optionally creating its own task, origin `xspace_request`), declines, or replies (`completed` / `failed`). No tool
    of the source creates tasks, runs or messages in the destination; the destination's task, inbox item and deciding run
    are never shown to the source.
  - **Knowledge goes to the KB scheduler, not to control.** A `knowledge` request names a KB slug and kind; in the same
    transaction it becomes a KB request of the destination (`kb_requests.xspace_request_id`) on a support session whose
    requester key is `space:<source>/run:<run>` (one session per source run, a `kb` run in the destination's own KB
    pool). The KB's allowed roles are for its own space's agents; the cross-space grant (with its KB list) is the
    admission. The source never reads the KB repository; the KB session answers with `spaceman_kb_answer` as for local
    requesters and the outcome is reported to the source instead of as a local message.
  - **Disclosure applies to what crosses back.** The destination's level for that peer, read when the reply is given:
    `none` — the outcome only (no reply text, no decline or failure reason); `summary` — reply text up to 2,000
    characters and the KB basis, no evidence; `detailed` — reply text up to 20,000 characters and KB evidence (paths
    and revisions). The reply is reduced before it is stored; what is cut is not kept anywhere. Attachments never cross.
    Notifications to the source carry no reply text: the source reads it with `spaceman_xspace_get`.
  - **Chains.** Every request records its correlation (the first request's ID), `caused_by`, depth and the spaces its
    chain visited. A request is caused by the incoming request named in `caused_by`; without it, by the only open
    incoming request of the source; with several open, the caller must name one or declare `independent` (audited).
    Depth above the destination's `max_depth` (default 3, at most 8) is refused, and so is any destination already in
    the chain (A→B→A, A→B→C→A). Omitting causation cannot reset a chain by accident, and evading it on purpose takes an
    explicit, audited claim; quotas bound the rest.
  - **Quotas.** Per pair per hour and open at once (the destination's row), and per source per hour to all peers
    (`max_outgoing_per_hour`, default 60). Refusals are `rate_limited` with text telling the agent it is notified, and
    every refusal (`not_found`, `forbidden`, `rate_limited`, depth, loop) is audited with outcome `denied`.
  - **Lifecycle and delivery.** States `pending` → `accepted` → `completed` / `failed`; `pending` → `declined`;
    open → `cancelled` (by the source) or `expired` (deadline 5 minutes to 7 days, default a day). The source is told of
    every outcome by a durable message to the requesting run while it lives, else to the space's control with the
    provenance (`requester_ended`), as for human responses (ADR-0032). The destination's control is told of
    cancellations and expiries; what it started stays its decision. Deadlines join the human-action timer as an extra
    deadline (one timer, kicked after commits; its first round after a start expires what passed while central was
    down). Everything is in SQLite; nothing polls.
  - **Events.** `xspace.request.created` and `xspace.request.state_changed` (metadata only: IDs, kind, state, depth,
    correlation) are published once for each of the two spaces with `side` `source` or `destination`.
- **Alternatives:** grants on the destination only (the source's operators could not stop their control from sending
  their data out); one combined "trust" flag (against SM-XSPACE-001); letting workers ask (any prompt-injected worker
  could exfiltrate); a source tool that creates tasks in the destination (SM-XSPACE-002 forbids it); KB requests routed
  through the destination's control (adds a model turn to an operator-preauthorized read; KB sessions already cannot
  orchestrate, SM-INV-013); filtering replies when read instead of when stored (the unfiltered text would sit in the
  shared record); copying attachments across spaces (no per-file disclosure model yet).
- **Consequences:**
  - The destination sees the source's request text and the requesting run's ID and role; that is what the source's
    outbound grant authorizes.
  - A disclosure level lowered after a reply was given does not change that stored reply (it was disclosed already).
  - Removing a peer row stops new requests at once; requests already open stay open, and when it was the destination's
    row, replies to them disclose nothing.
  - Purging either space deletes the requests between them.
  - No Terraform resources yet (M5-01 phase 2, done in ADR-0041); no real-harness evidence: the simulated suite and the
    dev stack (stub harness) only.

### ADR-0041 — Terraform provider phase 2: limits own only configured overrides, grants are owned whole, users are never deleted
- **Status:** Accepted, 2026-11-03 (M5-01 phase 2). Extends ADR-0035; no central API change.
- **Context:** SM-API-004 also lists limits, schedules, cross-space and auth configuration; SM-HUI-008 human-action
  policies; SM-LIMIT-001 the limits themselves. M4-02 (ADR-0036), M4-06 (ADR-0038) and the existing user and OIDC APIs now
  provide the endpoints. Several of them do not fit ADR-0035's default pattern (Optional+Computed fields, `If-Match` from
  private state): the limit overrides are one map replaced as a whole, the queue policy and a user's grants have no
  usable `If-Match`, a cross-space peer row is an authorization grant, users cannot be renamed or deleted, and a
  human-action policy `PUT` with version 0 silently overwrites.
- **Decision:**
  - **Resources (9) and data sources (2).** `spaceman_space_limits` (`GET/PUT /spaces/{id}/limits`),
    `spaceman_space_queue_policy` (`PUT /spaces/{id}/queue/policy`, read from `GET /spaces/{id}/tasks`),
    `spaceman_schedule` (`/spaces/{id}/schedules`), `spaceman_space_xspace_settings` (`/spaces/{id}/xspace/settings`),
    `spaceman_xspace_peer` (`PUT/DELETE /spaces/{id}/xspace/peers/{peer}`, read from the list),
    `spaceman_human_action_policy` (`/human-policies/{type}` or `/spaces/{id}/human-policies/{type}`), `spaceman_user`
    (`POST /users`, `/users/{id}/enable|disable`), `spaceman_user_grants` (`PUT /users/{id}/grants`) and
    `spaceman_user_identity` (`POST/DELETE /users/{id}/identities`); data sources `spaceman_space_limits` (effective
    values, defaults, overrides and sources) and `spaceman_user` (by ID or email). All are importable.
  - **Limits own exactly the configured overrides.** One Int64 attribute per overridable limit key (13; the top-level
    task limit lives in the queue policy and is its own resource), Optional and not Computed: null means "not
    Terraform's". Every write reads the current overrides and merges: configured keys are set, keys Terraform owned at
    the last refresh but no longer configures are removed (SpaceMan's installation default applies again), all other
    overrides — set in the UI or the API — are kept. Refresh fills only owned keys, so a foreign override is never drift
    and a cleared owned one is. `on_lower` follows the same rule (removing it resets it to `drain`). Destroy removes the
    owned overrides only. Import has no configuration to tell ownership, so the first read after an import takes every
    stored override (a private-state flag); a later plan clears those the configuration does not set. Writes send
    `If-Match` with the version read at refresh (none while nothing is stored).
  - **No `If-Match` available: compare before write.** The queue policy API has no version, and `PUT /users/{id}/grants`
    checks only the user's version, which grant changes do not bump. Both resources re-read the object just before writing
    and refuse with a "changed after the plan" error when it differs from the last refresh. This narrows, but does not
    close, the window a real precondition would close.
  - **Authorization grants are owned whole.** `spaceman_xspace_peer` has every flag Optional with a default (off,
    disclosure `none`, SpaceMan's quota defaults), so a flag turned on outside Terraform is drift and the next apply turns
    it off; a working grant needs two resources (the destination's inbound side and the source's outbound side, ADR-0038).
    `spaceman_user_grants` owns the user's whole manual grant set like `spaceman_space_accounts`; SSO-mapped grants are
    never touched. A create of a peer configuration that exists fails (`already_exists`), and so does a human-action
    policy create: the provider lists the scope first instead of letting version 0 overwrite (import it).
  - **Human-action policies.** All fields are Optional+Computed (ADR-0035). Because the most specific policy applies as a
    whole, fields left out on create start from the policy in force for that type and scope (the `effective` list; for
    `all`, the installation's `all` default or SpaceMan's built-in values), not from zeros.
  - **Schedules.** Full replacement through `PUT` with `If-Match`. `cron` and `interval_seconds` are owned (default empty
    and 0) so a kind change never keeps a stale timing field. An unconfigured `anchor` is kept while kind and interval
    stay the same and is unknown otherwise (SpaceMan then re-anchors at the update), mirroring the API. Occurrences are
    runtime state.
  - **Users.** Email and display name cannot be changed through the API (the store has a profile update, no operation
    exposes it): changes are refused at plan time (ADR-0035 "no silent replacement"); the email must be configured in
    lower case because SpaceMan stores it lower-cased. Destroy disables the user (no delete API), with a warning;
    SpaceMan's last-administrator rule still applies. Terraform never sends passwords, `mfa_required`, reset tokens or link
    invitations: the first are the user's own credentials, the rest are shown once.
  - **Singletons on destroy.** Cross-space settings are reset to SpaceMan's defaults (profile empty, depth 3, 60 per hour);
    the queue policy has no removal API and is kept, with a warning.
- **Alternatives:** a `values` map for limits (no per-limit documentation or typo protection); Optional+Computed limit
  attributes (a removed attribute could not clear its override, and destroy could not tell owned from foreign ones);
  ADR-0035 ownership for peer flags (a UI-widened grant would never show as drift); grants inside `spaceman_user` (JIT
  users created by OIDC could not get managed grants; two owners of one set); a central `PATCH /users/{id}` or an
  `If-Match` on queue policies and grants (central changes outside this work package; recorded as gaps).
- **Consequences:**
  - After importing `spaceman_space_limits`, overrides the configuration does not set are planned for removal.
  - A grant or queue-policy change made between the provider's re-read and its write is still overwritten (no
    server-side precondition).
  - Renaming a user needs a new user; destroying one leaves a disabled account behind.
  - Still not managed: space repositories (no API), the backup target and schedule (configuration file only), user
    profile edits and `mfa_required` after creation, type permissions beyond space enablement (no API), and a real
    upstream (Pulse, IdP) in the acceptance tests.

### ADR-0039 — Security review fixes: serialized credential attempts, daemon Git that runs no repository code, contained relocation paths (security-relevant change)
- **Status:** Accepted, 2026-11-03 (M4-08).
- **Context:** The M4-08 review (SECURITY_REVIEW.md) found four medium and fourteen low issues. The fixes below change
  security behaviour. All of them tighten it, and none relaxes a requirement, but some refuse things that worked before,
  so they are recorded as a **security-relevant change**.
- **Decision:**
  - **Credential attempts are serialized per throttle key** (`internal/auth/attempts.go`): password login (account
    key), the current-password check (account key), every second-factor check and TOTP confirmation (MFA key). The
    throttle check, the verification and the recorded failure therefore act as one step, and a burst cannot outrun
    the throttle (T-28). The lock is in-process because one central owns the database (SM-INV-001). Logins from one
    IP to different accounts are not serialized, so a shared NAT does not queue unrelated users.
  - **Idempotent requests are serialized per (principal, operation, key)**, so concurrent retries create once (SR-L01).
  - **The node daemon's Git runs no repository code** (T-26):
    - Writer worktrees are prepared with `core.hooksPath=/dev/null`, `core.fsmonitor=false` and the scrubbed Git
      environment, as relocation already was.
    - Before any Git command in an existing repository (memory checkouts, KB worktrees, writer worktrees, relocation),
      the daemon lists the repository's own configuration (scopes `local` and `worktree`). It refuses to run when a key
      would make Git run a program or reach an unchosen transport: filter, merge or diff drivers, `diff.external`,
      `core.sshCommand`, `core.gitProxy`, `core.askPass`, `core.pager`, `core.editor`, `core.alternateRefsCommand`,
      `sequence.editor`, `include.path`, `includeIf.*.path`, `credential.*`, `uploadpack.packObjectsHook`,
      `remote.*.uploadpack`, `remote.*.receivepack`, `remote.*.vcs`, `gpg.*program`, `protocol.*allow`,
      `url.*.insteadOf` / `pushInsteadOf` and `ssh.variant`.
    - The node's global and system configuration are the node operator's and stay in effect (SM-REPO-005).
  - **Relocation paths are contained** (T-27):
    - Central refuses inspection reports with worktree or repository paths outside the space folder.
    - The destination refuses worktree paths outside the space folder, in `instances/` or `dreams/`, or through a
      symlinked parent (checked before and after creating parents), and refuses a worktree folder that already exists.
    - The loose-file archive re-checks each symlink when it creates it and refuses `instances` whatever its type. The
      relocation walk skips `instances` whatever its type.
  - **Smaller authorization fixes:**
    - Purging a space removes its grants through the auth service, which reloads the engine after commit (SR-L02).
    - Only a recipient may reply to a message (SR-L05).
    - Deleting a transcript needs re-authentication with MFA, as deleting a recording does (SR-L06).
    - Recording a queue decision needs `queue:write` (SR-L07).
    - Event-template instructions may use only the template's variables (`vars.*` placeholders), and are rendered with the variables alone (SR-L08, T-29).
  - **Fault behaviour** found by the fault suite (`make test-fault`):
    - A recording write failure (a full disk) hands its `trunc` record (`disk`) and the run's close record to the
      shipper, so central's copy ends visibly. The quota `trunc` record is shipped too.
    - A writer resuming after local loss continues above central's acknowledged sequence.
    - A stopped agent ends its recorders.
    - A storage-busy API error answers `Retry-After: 1`.
- **Alternatives:**
  - For the throttle: a database reservation of the attempt before verifying. It is equivalent with one central and
    costs a write transaction per attempt, including successful ones.
  - For Git: disabling filters per name with `-c filter.<name>.clean=`. Names are unbounded, and this does not cover
    merge drivers, includes or URL rewrites.
  - Moving the memory checkouts' Git directories out of the agent's reach is a larger change to ADR-0025 and remains an
    option.
- **Consequences:**
  - A repository whose `.git/config` defines a filter, such as Git LFS installed with `git lfs install --local`, is
    refused for daemon Git operations: memory checkpoints of that checkout, worktree preparation and relocation. The
    error names the key. Installing LFS in the node's global configuration keeps it working. Recorded in LIMITATIONS.md.
  - One extra `git config` process runs before each daemon Git command in a repository.
  - Concurrent logins to one account wait for each other (each verification takes one Argon2id hash).
  - The open low and info findings, with their follow-ups, are listed in SECURITY_REVIEW.md.

### ADR-0040 — Remote CLI bound to the build's OpenAPI document by operation ID; secrets never as flag values
- **Status:** Accepted, 2026-11-03 (M5-02).
- **Context:** SM-API-003 asks for a CLI and a generated, documented contract; SM-API-001/002 for one administrative
  application layer with typed versioned schemas, If-Match, idempotency, pagination and RFC 9457 errors. `spaceman` had
  only local commands (`serve`, offline `admin`, `keys`, `backup`, `openapi`); operators had the UI, the raw API and
  Terraform. The API has ~240 operations and 300 schemas, all with `additionalProperties: false`.
- **Decision:**
  - **One binary, a remote client package.** `internal/cli` adds 22 remote command groups to `spaceman` (nodes, spaces with
    relocation, runs, inbox, tasks, human actions, schedules, limits, accounts, prompts, skills, harness profiles, roles
    and role templates, events, MCP servers and grants, approval policies, cross-space, users, tokens, audit, system
    with backups, `whoami`). They only call `/api/v1`; nothing bypasses the API, so authorization, audit and rate limits
    are the UI's (SM-API-001).
  - **Hand client, contract by operation ID instead of generated Go types.** Every command names the operation IDs it
    calls; method, path, path parameters, accepted headers and request-body properties come at run time from the
    OpenAPI document of the same build (built in process, as `spaceman openapi` prints it). Updates read the object,
    project it onto the operation's request schema (dropping read-only fields, filling required ones that a GET omits)
    and change only the flags given. A test fails when a command names an operation the document lacks, offers
    `--idempotency-key` where the operation takes none (or omits it where it does), or offers `--if-match` where the
    operation takes no If-Match. The client itself is small (~250 lines over `net/http` and JSON). Generating a typed
    client (oapi-codegen or ogen pinned in `tools/`) was rejected: tens of thousands of generated lines for 300 schemas,
    a network-fetched generator, and the commands would still need hand-written semantics (merging, version selection,
    secrets); the same reasoning as ADR-0035 for the Terraform client.
  - **Conventions.** `Idempotency-Key` (random, or `--idempotency-key`) on every create whose operation accepts one;
    those creates and GETs are retried on transport errors and 502/503/504, other mutations never. `If-Match` is the
    version read just before the change, or `--if-match` from an earlier read (a stale one is a conflict, never an
    overwrite). `--all` follows `next_cursor`. Problems are printed with code, field errors, operation and request ID.
    Exit codes: 0 ok, 1 usage, 2 API error / transport / unwanted final state, 3 conflict (409, 412, 428), 4 not found,
    5 authentication or authorization (401, 403, no token). `--yes` guards `node revoke`, `space purge` and
    `space relocate release` (more since the amendment below).
  - **Secrets (security).** The brief's `--token` flag is not provided: flag values are visible in the process list. The
    token comes from `--token-file` (refused when group- or world-readable), `SPACEMAN_TOKEN_FILE` or `SPACEMAN_TOKEN`.
    Every secret input (account secrets, passwords, Pulse `.creds`, MCP headers) is `--…-file` or `--…-stdin`; a test
    fails when any command has a value flag named after a secret. One-time secrets the server returns (enrollment and
    API tokens) are printed once, or written to a new `0600` file (`O_EXCL`) with `--secret-file` and removed from the
    output. The client logs nothing and never prints request bodies.
  - **Transport rules as in the Terraform provider.** Plain `http://` only to loopback hosts unless
    `--allow-insecure-http` (opt-in, documented as sending the token in clear text); redirects are never followed.
  - **Generated reference.** `spaceman docs cli --out site/reference/cli` (`make cli-docs`) renders one VitePress page
    per top-level command from the cobra tree, with each command's API operations; `TestCLIReferenceUpToDate`
    (`make check`) and `make site-check` fail when it is stale.
  - **API gaps closed.** Read operations that the CLI (and Terraform imports) need were missing for resources that had
    only a list: `get-event-connection`, `get-harness-profile` and `get-role-template` (ID or slug), `get-space-role`,
    `get-approval-policy` and `get-xspace-peer` (with ETag). They reuse the list operations' permissions. The raw routes
    outside the OpenAPI document (attachment content, transcript download) are now described in the API reference.
  - **Backups.** `spaceman system backup list|create|verify` calls a running central; the existing offline
    `spaceman backup` commands (and offline restore) are unchanged.
- **Alternatives:** a generated typed client (above); a separate `spacemanctl` binary (a second artifact to version and
  ship for no isolation benefit: the client holds no more than the token); a `--token` flag "for convenience" (leaks
  through `ps` and shell history); following redirects within the same host (still lets a proxy move the token to
  another path or scheme); retrying every create (could create duplicates where the API has no idempotency key).
- **Consequences:**
  - The CLI must come from the same release as the server; an older client may name operations a newer server renamed
    (the API is versioned under `/api/v1`, so this is a compatibility break the API itself would have).
  - Each remote invocation builds the OpenAPI document in process (~60 ms).
  - Commands cover the operator surface (132 of the 248 operations), not every operation: KB requests and
    proposals, memory and its conflicts, messages, dreams, OIDC providers and mappings, human-action types and policies,
    quarantine, sessions, MFA and the root token have no command yet and stay reachable through the API, the UI or
    Terraform. Listed in PLAN.md M5-02. *(Superseded by the amendment below: 239 of 248.)*
  - API tokens are shown once by `token create` unless `--secret-file` is given; the root token cannot create them.
- **Amendment (2026-11-03, M5-02 completion): every scriptable operation has a command.**
  - **Coverage.** 107 more operations got commands, so 239 of the 248 operations are reachable from the CLI. New groups:
    `kb` (with `request cancel`, `proposal accept|reject`), `memory` (collections, sync status, files, history,
    conflicts), `message`, `dream`, `approval` (list, get, revoke; deciding stays `human respond`), `attachment`
    (upload, get, download), `oidc` (providers, group mappings, test-login preview, sign-in providers), `identity`,
    `session`, `mfa`, `password`, `watch`. Existing groups gained: `task block|unblock|escalate|depends|decisions|set-policy`,
    `run input|usage|recording|transcript`, `space usage`, `human create|assign|comment|move|route|approval` with
    `human type` (catalog and a space's enabled types) and `human policy` (installation defaults and space overrides),
    `account space|set-space`, `mcp space-servers`, `role options`, `prompt diff|discard-draft|fork|rollback`,
    `skill create|draft` (from a directory), `event template preview`, `event subscription quarantine
    list|reprocess|discard`, `user reset-token|link-invite|identity`, and `system outbox|restore|break-glass|root-token`.
  - **Coverage is enforced.** `cli.Excluded` lists the operations without a command, each with its reason;
    `TestEveryOperationHasACommand` fails when an operation of the build's document has neither a command nor an
    exclusion, when an excluded operation gains a command, or when an exclusion names an operation the document lacks.
    The generated reference lists the exclusions.
  - **Exclusions (9), all browser sign-in flows:** `login` (creates a browser session with a cookie and CSRF token; the
    CLI uses API tokens), `logout` (ends the calling browser session; a token has none, `token revoke` revokes tokens),
    `verify-mfa` (second step of an interactive sign-in), `reauth-password` and `reauth-totp` (step-up of a browser
    session; tokens are not subject to re-authentication), `recovery-login` (break-glass sign-in that creates a
    browser session), `start-oidc` and `oidc-callback` (browser redirects to and from the identity provider), and
    `reset-password` (public and token-less; central accepts an anonymous state-changing request only from its own web
    origin, and the CLI does not forge an `Origin` header to pass that check, so the user redeems the token that
    `user reset-token` issues on the sign-in page). Of the raw routes, the live terminal stream has no command: it
    replays terminal output with its escape sequences, which belongs in the web terminal viewer
    (`run recording download` saves the recording instead). The change feed is followed with `spaceman watch`
    (`--after`, `--count`; JSON Lines with `-o json`).
  - **Conventions added (security).** One-time secrets of the new commands are shown **only on request**: `user
    reset-token`, `user link-invite` (the URL carries the token), `mfa enroll` (the otpauth URI carries the TOTP
    secret), `mfa confirm` and `mfa recovery-codes` (recovery codes) and `system root-token rotate` refuse to run
    without `--secret-file PATH` (new `0600` file, `O_EXCL`; every response field that reveals the secret is replaced) or
    `--show-secret` (printed once). `node create`, `node enrollment` and `token create` keep their documented behaviour.
    New secret inputs are files or standard input only: `--client-secret-file` (OIDC), `--code-file`/`--code-stdin`
    (TOTP), `--current-password-file`/`--new-password-file`. Downloads (`attachment download`, `run recording download`,
    `run transcript download`) and `memory file --out` write to `--out` only: a new `0600` file that appears under its
    name complete (temporary file and hard link), never replacing an existing one without `--force`; `--out -` streams
    to standard output only when it is not a terminal. `memory file` without `--out` prints the agent-written text with
    control characters removed. `--yes` now also guards `run recording delete`, `run transcript delete`, `oidc provider
    delete`, `system root-token rotate|revoke`, `mfa recovery-codes`, `identity unlink`, `user identity unlink`, `event
    subscription quarantine discard` and `prompt discard-draft`.
  - **Updates without a get operation.** Dream configurations and human-action policies have no single-item read, so
    their updates read the list, merge the flags given into the matching item and send its version as If-Match (a policy
    that is not stored yet starts from the policy in force, without If-Match). The queue policy carries its version in
    the body; `task set-policy` reads it just before the change. Knowledge base updates merge the `kb` object of the
    detail read. Downloads and the change feed use the raw routes (`site/reference/api.md`), which have no request
    timeout and are never retried; uploads are multipart forms with an Idempotency-Key.

### ADR-0042 — Core NATS subscriptions as a fixed, non-durable delivery mode; ingestion metrics with fixed-enum labels
- **Status:** Accepted, 2026-11-03 (M2-03).
- **Context:** SM-EVT-011 asks for JetStream durability where supported and for NATS Core ingestion "labeled
  non-durable"; SM-EVT-013 and SM-OBS-002 ask for lag and error metrics with bounded labels. Some sources publish plain
  NATS messages that no stream captures. go-pulse v0.1.3 has no core subscription call; its contract (and Pulse's
  `subscribe` grant) points to `Client.Conn()` with subscribe grants (`docs/contracts/pulse.md`). The SDK's
  `Subscription` exposes no pending counts.
- **Decision:**
  - **A subscription property, not a connection kind.** `delivery` is `durable` (default, unchanged behaviour) or
    `core`, set at creation and immutable (a changed mode would change what the history and the cursor mean). Both use
    the same Pulse connection and credentials; a core subscription subscribes on `pulse.Client.Conn()` and needs
    `subscribe:<subject>` instead of a durable grant. A separate `nats` connection kind (credentials without the Pulse
    client tag) was not added: nothing needs it yet, and Pulse issues every credential.
  - **The same path.** Core messages are decoded with `envelope.DecodeMsg` and go through the loop guard, the rate
    limit, `Process` (binding, filters, schema, quarantine, rendering, inbox dedup on `<subscription>:<event id>`) and the
    delivery history unchanged. Subjects keep the same validation (under `evt.>`, no variable wildcards, SM-EVT-010).
  - **Identity without an ID.** A well-formed `Nats-Msg-Id` (`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`) is the event ID;
    otherwise `sha256-` + the first 128 bits of a SHA-256 over the length-prefixed subject, `ce-type`, `ce-source`,
    `ce-time` and payload. `ce-time` is included so that a source re-sending a message deduplicates while repeated alerts
    with their own time stamps do not; identical messages with no ID and no time are one event (documented). A missing
    `ce-time` is replaced by the receipt time.
  - **Honest failure modes.** Nothing is acknowledged. A transient failure, a slow-consumer drop, a disconnection or a
    message over the ingestion rate loses the message; the subscription's last error says so, a warning is logged (IDs
    only) and the metrics count it. Every core delivery has origin `live` (core NATS never redelivers; a repeated ID is
    the source sending again, recorded as a duplicate). Replay, start-now and any `start` on update are refused with
    `precondition_failed`; creation accepts only `start.policy = new`; a subject change resubscribes without a cursor.
  - **Refused subscriptions are visible.** A permission violation is not an error of `Subscribe`; the manager flushes
    after subscribing and checks for a new `Permissions Violation for Subscription to "<subject>"`, and a connection error
    handler catches late ones. Either marks the subscription `error` with the grant it needs and retries it on the next
    round (not at once, to avoid a tight loop while the grant is missing).
  - **Labelled everywhere.** The API schema documents `delivery` as non-durable for `core`; the UI shows a
    "core NATS · non-durable" badge with the explanation on the subscribe form, the subscription list and page (where
    the cursor menu is disabled); the guide has a durable-versus-core table.
  - **Metrics.** `spaceman_ingest_*` families with labels from fixed enums only (`delivery`, `origin`, `result`,
    `state`, `connection_kind`); never subscription, space or event IDs or subjects. Lag comes from the JetStream API
    (`JetStream().Consumer(...).CachedInfo()`: `NumPending`, `NumAckPending`), summed over subscriptions, refreshed with
    the ingestion manager's rounds (changes and every 30 s); core pending and drops come from nats.go `Pending()` and
    `Dropped()`. Catalog in `site/operators/observability.md`, pinned by `TestCatalogGolden`.
- **Not a security relaxation.** The default stays durable, and GOAL SM-EVT-011 already provides for labelled
  non-durable NATS Core ingestion; nothing about authorization, credentials or authority changes (event content stays
  untrusted, and the grant needed is narrower than a durable one).
- **Alternatives:** an ephemeral (ordered) JetStream consumer for "core" sources (needs the stream, which such sources do
  not use); a `nats` connection kind (above); in-process retries for core messages (would hold the subscription's single
  handler goroutine and grow the client buffer under a storage outage, turning one lost message into many); per-
  subscription lag gauges (unbounded labels).
- **Consequences:** core subscriptions lose messages by design and say so; content-hash dedup can merge genuinely
  repeated identical messages without an ID or time; the durable lag gauge costs one consumer-info request per
  subscription and round; rate-limited events are counted with origin `live`.

### ADR-0043 — A space's lease guard ends with its last live run
- **Status:** Accepted, 2026-11-03 (traceability audit; amends ADR-0017).
- **Context:** ADR-0017 starts a lease-guard terminal "for each space with active runs". The node agent started the
  guard at a space's first launch and never ended it, so every space that had ever run kept a process on its node that
  read the lease file once a second for as long as central kept renewing the lease. That is a permanent polling loop
  per sleeping space, which SM-ARCH-004 forbids.
- **Decision:**
  - When a run of a space ends, the agent checks the runner for the space's live managed terminals. If there are none,
    it kills and removes the space's guard terminal. A space without live runs keeps no guard.
  - A launch starts a new guard (`ensureGuard`) before its run and starts the run under it. A per-space lock is held
    from `ensureGuard` until the run is started, and the retirement takes the same lock, so a guard is never ended
    under a starting run.
  - Nothing else changes: the guard's file check, the lease file, the parenting of runs and the dead-daemon bound are
    as in ADR-0017.
- **Not a security relaxation.** Enforcement is unchanged for every live run: each one still runs under a live guard.
  The guard is retired only when it has no live runs left to end.
- **Alternatives:** keep the guard and accept one 1-second file check per idle space (contradicts SM-ARCH-004 and
  ADR-0017's own scope). Make the guard wait on a file-change notification instead of polling (the guard has to
  notice lease *expiry*, a moment in time rather than a file change, so it would still need a timer).
- **Consequences:** a space's first launch after an idle period starts two terminals (guard and run), as its very first
  launch did. A guard left by an older agent is retired when that space's next run ends. Tested with the fake runner
  (`internal/app` `TestIdleSpaceKeepsNoLeaseGuard`). The real herdr-core suite still covers the guard ending a dead
  agent's runs (`TestRealNodeEndToEnd`), but it does not cover retiring the guard.

### ADR-0044 — Configured space repositories: a central list, node-side materialization and reports, node-owned Git authentication
- **Status:** Accepted, 2026-11-03 (gap closure for SM-REPO-001/003, SM-SPACE-001, SM-UI-002, SM-API-004; migration 0029,
  node protocol minor 4).
- **Context:** repositories were implicit folders in a space folder on the node. Central kept no list, so there was no
  repository API, page or Terraform resource; remotes, dirty state and unpushed commits were inspected only during a
  relocation; and only knowledge bases named a repository folder (`kb_repos.repo`). SM-REPO-005 says Git authentication
  is the node's and SpaceMan never requires managed Git credentials.
- **Decision:**
  - **A central list per space** (`space_repositories`): name (unique, fixed), folder (`path`, unique, fixed, relative,
    outside `instances/`, `worktrees/`, `work/`, `dreams/`), clone URL, default branch, role (`code` or `kb`) and clone
    policy (`auto`, `on_demand`, `manual`; a repository without a URL must be `manual`). At most 50 per space. API CRUD
    under `/spaces/{id}/repositories` with `If-Match` on updates (optional on delete), audited as
    `space_repository.*`, recorded in the change feed; reads need `space_config:read`, changes and syncs
    `space_config:write` (repositories are space configuration; no new permission object).
  - **No credentials, anywhere.** URLs with user information over http(s), with a password over ssh, Git's
    remote-helper syntax (`<transport>::…`, e.g. `ext::`), option-like values and whitespace are refused. Production
    accepts `https://`, `ssh://` and scp-like addresses; development mode also `http://`, `git://`, `file://` and
    absolute paths (local remotes for tests). The same validator (`proto.ValidateRepoURL`, local allowed) now also checks
    relocation repository URLs. Clones and fetches run with the node's own Git configuration and credentials (SSH,
    credential helpers, Hallpass Git roles), as worktrees and relocation already did. Account- or secret-backed Git
    credentials are deliberately not offered: they would make SpaceMan a Git credential store, which SM-REPO-005 excludes.
  - **The node materializes.** A new space-level command `space.repos.sync` (lease required) carries the space's list,
    which repositories to clone when missing and which to fetch. The node clones
    (`git clone --origin origin [--branch <default>]`, then one fetch), fetches from the configured URL into the matching
    remote's tracking refs
    (`+refs/heads/*:refs/remotes/<remote>/*`, no prune, no tags), asks the remote for its default branch (`git ls-remote`,
    30 s), and inspects every repository with relocation's inspection (remotes redacted, branches with unpushed commits,
    changes, untracked files, stashes, worktrees). It reports `repos.status`, journaled with the command so a repeated
    delivery re-sends it. A launch whose worktree names a configured repository carries it (`LaunchPayload.Repository`),
    so the node clones it first when missing (first use). Git runs as for relocation: node environment without
    repository-redirecting variables, hooks and fsmonitor off, `protocol.ext.allow=never` (added to all daemon project
    Git), no Git at all in a repository whose own configuration would run a program (`memory.UnsafeConfig`). A folder in
    the way is never overwritten; nothing is reset, merged, pushed or deleted (SM-REPO-006).
  - **Reports follow events, not a clock** (SM-ARCH-004): a repository created or edited (clone of `auto` ones), the node
    connecting (after its leases are granted), a run of the space ending (debounced 30 s while unanswered) and an
    explicit sync (`POST …/repositories/sync`, which also clones `on_demand` ones and fetches). Central keeps the latest
    report per space apart from the configuration (it never changes a repository's version), accepts it only from the
    space's current node, keeps only configured repositories, bounds branches, orders reports by the node's check time,
    and marks a report from an earlier node as stale. Worktree ownership is the latest run SpaceMan launched in that
    worktree folder.
  - **Worktrees and knowledge bases use the list.** A worker's `repo` may be a configured repository's folder or name;
    unlisted folders keep working (the report lists them as unlisted). A knowledge base keeps naming a folder; any folder
    no repository uses is registered as a `kb`, `manual` repository in the same transaction, and migration 0029 does the
    same for existing knowledge bases (one repository per space and folder, a name suffix on collision), so existing KB
    configuration keeps working. A repository a knowledge base uses cannot be removed. Knowledge bases do not accept a
    repository's name, so what Terraform configures is what is stored.
  - **Relocation reads the list.** A clean relocation's destination also clones configured `auto` repositories the
    source never cloned; a relocation from an unavailable source clones every configured repository with a URL besides
    the operator's list (a listed folder wins).
  - **Surfaces.** A space **Repositories** page (list, report, worktrees with their runs, unlisted folders, add, edit,
    remove, sync), worktree links on runs and tasks, a read-only agent tool `spaceman_repos_list` (control, worker, KB,
    diagnostic), `spaceman repo list|get|create|update|delete|sync`, and the Terraform resource
    `spaceman_space_repository` (configuration only; reports and syncs are runtime state).
- **Not a security relaxation.** No credential is stored or accepted; URL validation is new and stricter for relocation;
  Git hardening is extended (`protocol.ext.allow=never`). The node now runs network Git (clone, fetch, ls-remote) for
  configured repositories with the node's credentials. Operators with `space_config:write` can therefore make the node
  clone a URL its credentials reach; agents on that node already have the same credentials in their terminals
  (SM-REPO-005), and repository content stays untrusted.
- **Alternatives:** repositories as a field of the space (no per-repository versions, worse Terraform ergonomics);
  periodic polling of every space's repositories (SM-ARCH-004); credentials as SpaceMan accounts (SM-REPO-005); making
  knowledge bases reference a repository ID (a breaking API and Terraform change, and a table rebuild).
- **Consequences:** a report can be minutes old; it says when it was made. ls-remote needs a reachable remote at each
  report. The first launch of an uncloned repository waits for the clone. Nodes older than protocol 1.4 keep the
  configuration but get no syncs (an explicit sync says to upgrade). Worktrees are still never removed automatically.

### ADR-0045 — Retention as limits, plaintext space exports, a resumable audit log, memory flushed before backups and Hallpass sign-in parity (security-relevant change)
- **Status:** Accepted, 2026-11-03 (requirement gaps SM-REC-003, SM-HUI-007, SM-ATT-006, SM-SPACE-001, SM-OBS-003,
  SM-MEM-005, SM-AUTH-001; migration 0030).
- **Context:** Central kept recordings, transcripts, attachments and human actions until someone deleted them or the space
  was purged, and recordings had a fixed 512 MiB bound. There was no export. The audit log could only be paged newest
  first and was never pruned, so a consumer had no durable position and no gap signal. Backups held only the memory
  central had acknowledged, without asking nodes to flush first. Hallpass's SSO-only enforcement, direct-provider sign-in
  and RP-initiated logout were not ported (ADR-0029). Retention deletes data, an export takes data out of SpaceMan, and
  SSO-only turns a sign-in path off, so this is recorded as a **security-relevant change**.
- **Decision (retention):**
  - Retention periods are limits in the one limits model (ADR-0036): `retention_recordings_days`,
    `retention_transcripts_days`, `retention_attachments_days` and `retention_human_actions_days` (unit days, 0 keeps,
    0–36,500), with installation defaults in `limits:` (all 0) and per-space overrides in `space_limits`. No separate
    policy table. Audit retention is installation-wide only (`retention.audit_days`, 0 or 30–36,500): the hash chain is
    one chain for the installation.
  - Changing a space's retention override needs the new `retention:write` permission (admins only) on top of
    `space_config:write`, because a shorter period deletes data: operators could otherwise delete recordings, which needs
    `recording:delete` (admins).
  - One daily pass (`internal/retention`) runs on central's shared deadline timer (`human.Deadline{Name: "retention"}`):
    due ten minutes after the first start, then a day after the previous pass started. Nothing polls per space. A pass is
    recorded in `retention_passes`, audited (`retention.started`, `retention.finished`) and in the change feed;
    passes interrupted by a restart are marked `interrupted`. Administrators can run one at once (`POST /retention/run`,
    step-up `system:write`).
  - What it deletes, per space, once the period passed: central's copy of an ended run's recording (counted from
    `ended_at`, marked in `runs.recording_deleted_at`); a stored transcript; an attachment whose referencing inbox items
    are all done or rejected, unchanged for the period, and neither linked to nor the origin of an open task or human
    action (an attachment with an owner of another kind is kept); a resolved, cancelled or expired human action (with its
    comments and notifications) that no open task, blocker, open follow-up or open inbox item refers to and whose
    response notifications were all acknowledged. Candidates are re-checked inside the deleting transaction. Unreferenced
    uploads stay with the hourly collection.
  - Every deletion is audited (`recording.deleted`, `transcript.deleted`, `attachment.deleted`, `human_action.deleted`,
    `audit.pruned`; actor `system:retention`, details with the reason and period) and recorded in the change feed in the
    same transaction. Files are removed before that transaction (a crash in between only leaves an unmarked row that the
    next pass marks); attachment content is removed afterwards by the collection. No I/O in a write transaction.
  - Nothing is deleted while a restore is not acknowledged: the pass is not scheduled and a manual pass is refused (409).
  - Deleting a recording through the API now also needs the run to have ended and is audited and in the change feed.
  - Audit pruning deletes events older than the period in batches, never the newest event, and records the sequence and
    hash of the last deleted event in `audit_prune`. Verification (also of backups) starts from that hash; the backup
    check compares the head sequence and hash instead of the event count.
- **Decision (audit cursor):** `GET /audit?after=<seq>` lists events oldest first after a durable position;
  `next_after` is the position to store. When retention pruned events after the position the response carries
  `gap: {after, oldest}` like the change feed's gap event. Every page carries the pruned prefix (`pruned`), which the
  audit page shows. Newest-first paging with `cursor` is unchanged.
- **Decision (recording size):** `recording_quota_bytes` (1 MiB–64 GiB, default 512 MiB) is a space limit. Central sends
  it with each launch (`LaunchPayload.recording_max_bytes`, an optional field: older agents ignore it and keep 512 MiB);
  the node keeps it beside the run, so a recorder restarted after an agent restart applies the same bound.
- **Decision (export):** a space export is one ZIP of JSON Lines (inbox items, tasks with dependencies and blockers,
  human actions with requests, responses and comments, messages, attachment metadata and references), the attachment
  files, `space.json`, `README.txt` and `manifest.json` (format `spaceman.space-export/v1`, counts, every file's size and
  SHA-256). It is **plaintext, not sealed like backups**: an export exists to take data out of SpaceMan, and an archive
  sealed under the KEK could only be read by SpaceMan itself. The warning is in the API summary, the archive, the UI
  and the CLI help, and an `X-Spaceman-Export-Encrypted: false` header. It holds no credentials, tokens or sealed fields
  (no such table is read; binary columns are left out). Only space admins export (new `space:export`); each export is
  audited (`space.exported`). It is streamed from one read transaction (a consistent snapshot) straight to the
  requester, so central keeps no plaintext copy; `GET /spaces/{id}/export/summary` estimates its uncompressed size, an
  export over the space's `export_max_bytes` (default 2 GiB) is refused before it starts, and the stream stops if the
  archive grows past it. Recordings and transcripts are not included (they have their own downloads).
- **Decision (role memory before backups, SM-MEM-005):** node protocol minor 5 adds `memory.flush` (central → node:
  `flush_id`, `space_ids`, `wait_seconds` ≤ 600) and `memory.flushed` (node → central: per space `flushed`,
  `collections`, `detail`). It is not a durable command: nothing is journaled, no lease is needed, and the node does only
  what it would do on its own (checkpoint every instance, sync each collection). Before every scheduled or manual backup,
  before the database snapshot and outside any transaction, the backup service calls `PrepareMemory`, which asks every
  connected node with minor ≥ 5 about its non-archived spaces and waits at most `BackupMemoryWait` (one minute) plus a
  margin of 1–10 s. A space counts as flushed only when its node reports that every collection had no uncheckpointed
  files, no unacknowledged commits and no sync in flight; otherwise it is recorded as possibly stale with a reason
  (`node_disconnected`, `protocol_too_old`, `timed_out`, `not_acknowledged`). Answers from another node or for another
  flush are ignored. The backup never waits beyond the bound and never fails because of the flush. The manifest records
  `memory_flush` (attempted, flushed spaces, stale spaces with node and reason) inside the existing sealed, checksummed
  manifest, adds a warning naming stale spaces, and `backup.finished` audits the counts. Offline CLI backups record that
  no flush was attempted. Older manifests without the field still verify.
- **Decision (sign-in parity with Hallpass, SM-AUTH-001):** ported from go-hallpass `bdd8329` (`internal/oidc/admin.go`,
  `internal/opauth`, `internal/httpapi/h_{auth,sso}.go`); this lifts the "not ported" list of ADR-0029.
  - *SSO-only enforcement*: the `auth.local_login_disabled` setting refuses password sign-in; recovery (break-glass)
    logins are unaffected. It is set only through `PATCH /oidc/settings` (step-up `user:write`), never through the
    configuration file. Turning it on requires an enabled provider with a recorded successful test login and at least
    one enabled global administrator holding a manual admin grant with a linked identity on an enabled provider.
    SSO-mapped admin grants do not count because they are re-derived at every SSO sign-in (ADR-0029); this deviates from
    Hallpass, which counts every administrator role. While it is on, SpaceMan refuses, in the same transaction, any
    change that would break that precondition: disabling, deleting or changing the issuer or client of the last tested
    provider, unlinking the last SSO administrator's identity, removing that administrator's grant or disabling them
    (`ErrLastSSOAdmin`, Hallpass SR-06 and HP-AUTH-015). Turning it off needs no precondition. Both changes are audited
    (`auth.local_login.changed`). It is installation-wide, as in Hallpass; there is no per-user flag.
  - *Direct provider*: `auth.direct_provider` names one enabled provider (audited `auth.direct_provider.changed`),
    ignored when that provider is disabled or deleted, and exposed on the public `/system/info`. The sign-in page
    continues to it, except at `/login?password=1` and after a sign-out, so signing out never signs straight back in.
  - *RP-initiated logout*: per provider `end_session_enabled` (migration 0030). `end_session_endpoint` is read from
    discovery and validated like the other endpoints (https, the issuer's site unless cross-origin endpoints are
    allowed, private-network refusal); it is optional. Signing out of an SSO session returns `provider_logout_url` =
    endpoint + `client_id` + `post_logout_redirect_uri` (`<public_url>/login`), and `POST /auth/logout` answers 200 with
    that body instead of 204. No `id_token_hint` is sent, because ID tokens are not retained (Hallpass). The UI offers
    it as an extra step and follows only https (or localhost http) URLs; it ends the session at that provider only.
  - *Porch*: v0.3.0 ends its own session only with an `id_token_hint`; without one it only redirects to the registered
    post-logout URI (`docs/contracts/porch.md`). Against Porch, provider sign-out therefore leaves the Porch session.
- **Alternatives:** a separate retention policy table (a second model next to limits for the same kind of per-space
  number with an installation default; rejected). Per-space timers (contradicts SM-ARCH-004). Sealing exports under the
  KEK like backups (unreadable outside SpaceMan, so not an export) or with an export-specific passphrase (a new
  construction; rejected under the crypto rule). Storing exports on central for later download (a plaintext copy at
  rest). Keeping ID tokens to send `id_token_hint` (Hallpass does not retain them; a credential-like artifact at rest).
- **Consequences:**
  - Shortening a retention period deletes data at the next pass; it is admin-only, audited, and recoverable only from a
    backup. The node's own recordings are never pruned by SpaceMan.
  - Exports are as sensitive as the space's content and are the operator's to protect once downloaded.
  - A SIEM can follow the audit log with a stored position and learns when retention outran it.
  - A backup still guarantees only the history central acknowledged, but it now says which spaces might be missing
    newer memory. Agents older than minor 5 still connect (at minor 3 or 4) and their spaces are reported stale.
  - An installation can be made SSO-only without an administrator lockout. Provider sign-out works only with providers
    that honour hint-less logout.

### ADR-0046 — UI gap closure: terminal takeover, clarification notices, attention summary, per-user notification preferences (security-relevant change)
- **Status:** Accepted, 2026-11-03 (migration 0031; SM-UI-004, SM-HUM-006, SM-UI-003, SM-HUI-006, SM-ATT-001, SM-EVT-017).
- **Context:** the web UI could not take over a terminal, ask a requester for more information, gather what needs a
  person, or let a user choose notifications; only inbox items carried attachments. herdr-core 0.1.16 offers
  `terminal.send` (paste plus Enter), `terminal.keys` (named keys only: `"`, `\`, `$` and `~` are refused) and
  `terminal.attach` with `observe:false`, verified to type raw bytes (contract S-3 and S-5).
- **Decision — terminal takeover (security-relevant: it opens operator keyboard input to agent terminals):**
  - A takeover is a durable `run_takeovers` row held by one user (or root), started only with `terminal:takeover` in the
    space (admins by default) plus a recent sign-in for browser sessions. At most one active takeover per run.
  - The node opens an interactive attach at the terminal's current size on its own herdr-core connection, and closes
    that connection to detach. Keystrokes travel `takeover-input` → central → ephemeral `takeover.input` → attach,
    bounded to 4096 bytes per request and 50 requests or 64 KiB per second per takeover, never stored, logged or
    audited. Node protocol minor 6; older nodes are refused takeover.
  - While a run is taken over, central refuses `send-input` and terminal notices (`ErrTakenOver`) and the node refuses
    `run.input`, the one command through which all automated input passes. Held notices go out after release. Stop and
    interrupt still work.
  - It lasts 30 s per renewal (the UI renews every 10 s) and ends on release, on another permitted operator's end, on
    expiry (closed page, revoked session or token: bounded at 30 s; a revoked holder cannot type at once because every
    input request is re-authorized), on run end, or when the node's attach breaks. It survives a central restart until
    expiry. Start and end are audited with holder, reason, duration and bytes typed; every viewer of the run sees the
    holder.
- **Decision — clarification requests:** `POST …/human-actions/{id}/clarify` stores a `clarification` comment (comments
  now have a kind: comment, clarification, reply) and, for an agent requester, a `clarification` notification routed
  exactly like a response (`human_notifications.kind` widened; the table is rebuilt with its columns in order). The
  action stays pending; a clarification is never a response. The requester's run (or its instance, or control) answers
  with the `spaceman_human_reply` tool; other runs are refused, as they are from the response.
- **Decision — attention summary:** `GET /attention` is computed on request with one grouped query per category, plus
  the fleet's in-memory node connections and the cached readiness report; there is no background loop. Each category
  is included only where the caller holds its read permission (installation-wide sections need `system:read`, the node
  list `node:read`). Clients refresh it from the change feed; storage health has no change event.
- **Decision — notification preferences:** stored per user (`notification_prefs`, If-Match versions, audited), they
  decide only what the open web UI announces (in-app and, with the browser's permission, desktop notifications). The
  UI reads a changed action once per change-feed entry; nothing polls. SpaceMan sends no e-mail or chat notifications.
- **Decision — attachments beyond the inbox:** tasks (at creation), human-action requests, responses and comments, and
  messages and replies reference attachments through the existing `attachment_refs` (owner kinds `task`,
  `human_action`, `human_response`, `human_comment`, `message`). Every referenced attachment must belong to the item's
  space (checked in the write transaction), and content stays served by the space's `attachment:read` route, so
  authorization matches the owning item's space as for inbox items.
- **Interpretation — custom definition editor (SM-EVT-017):** the subscribe dialog's custom path is a JSON editor with
  structural validation as you type, server validation and a sample-event preview. It is the custom editor GOAL asks
  for; a field-by-field visual editor is not built (LIMITATIONS).
- **Security notes:** takeover keystrokes may contain secrets; they cross only the node WebSocket and appear in the
  recording only as whatever the program echoes. Automated input stays paused until a revoked holder's takeover expires
  (at most 30 s). Clarification notices reveal the question only to runs related to the action. The attention summary
  reveals counts only for readable spaces.
- **Alternatives:** takeover through `terminal.keys` (cannot carry arbitrary text) or `terminal.send` (always presses
  Enter); clarification as a plain comment (does not reach an agent requester); a polled health summary (forbidden by
  SM-ARCH-004); browser-only notification settings (not per user).
- **Consequences:** migration 0031; node protocol 1.6; tests `internal/app` `TestTerminalTakeover`, `TestAttentionOverHTTP`,
  `TestHumanSearchClarifyAttachmentsAndPrefsOverHTTP`, `internal/human` `TestClarificationRequest`,
  `TestSearchFiltersAndSorts`, `TestActionAttachments`; real `TestRealTakeover` and `TestRealTakeoverTypesAndDetaches`
  (hc 0.1.16).

### ADR-0047 — The Terraform provider lives in its own repository
- **Status:** Accepted, 2026-11-03 (operator decision; supersedes the layout part of ADR-0035).
- **Context:** ADR-0035 put the provider in this repository as the nested module `terraform-provider-spaceman/`, with
  its unit tests in `make check` and `tf-*` Makefile targets. The operator decided the provider belongs in its own
  repository next to this one. Keeping Makefile targets that point at a sibling checkout would break `make` for anyone
  who checks out this repository alone.
- **Decision:**
  - The provider is the repository `terraform-provider-spaceman` (checked out as `../terraform-provider-spaceman` beside
    this one), Go module `github.com/aurimasniekis/terraform-provider-spaceman`. Its history was carried over with
    `git subtree split`, so its commits keep their messages and dates.
  - This repository no longer contains or refers to it in its build: no `tf-*` targets, nothing in `make check`, no CI
    step. The provider repository has its own Makefile (`test`, `lint`, `acc`, `docs`), lint configuration and CI.
  - The provider's acceptance tests build `spaceman` and `devpulse` from a SpaceMan checkout: `SPACEMAN_SRC`, or
    `../spaceman` next to the provider repository. A provider change that needs a new SpaceMan API needs the matching
    SpaceMan checkout; the API contract remains this repository's `api/openapi.json`.
- **Consequences:** a change to the administrative API is no longer caught by this repository's `make check` if it
  breaks the provider; run the provider's `make acc` against the change. Traceability rows that cite provider tests
  name tests in that repository.

### ADR-0050 — Incident policy (trusted triggers, worker handling, resumption), run:diagnose, human-action type permissions and user profile edits (security-relevant change)
- **Status:** Accepted, 2026-11-03 (gap closure for SM-QUEUE-009, SM-CTRL-004, SM-HUI-008 and the user part of SM-API-004;
  migration 0034, no node protocol change).
- **Context:** The incident policy set insertion, preemption, the active limit and escalation reordering, but any caller
  that could create a task could raise an incident, a preempted task's workers kept running without anyone being told,
  and nothing defined resumption. `run:diagnose` existed but diagnostic launches were checked with `run:control`.
  Human-action types had no permissions beyond space enablement, and a user's email, display name and `mfa_required`
  could not be changed after creation, so Terraform refused those changes at plan time (ADR-0041).
- **Decision:**
  - **Trusted triggers.** A space's queue policy lists the sources trusted to raise incidents: `operator` (a person
    through the API or UI, including the root token), `control` (the space's control on its own judgement),
    `subscription:<id>` and `template:<id or slug>` (an event delivered by that subscription or rendered by that
    template). Default: operator and control (what spaces had before). The trigger is where the request comes from:
    an operator's call is `operator`; control's own request is `control`; control creating an incident from an inbox
    item takes the item's source: an event's subscription and template, an operator's item `operator`, and the source
    kind (worker, dreamer, kb, cross_space, webhook, schedule, human_response) for everything else. Those kinds can
    never be listed, so a worker or another space can never raise an incident, even through control. An untrusted
    request is, per policy, **proposed** (default: created as an ordinary task, inserted by priority, nothing preempted,
    marked `incident_proposed_by`, audited `task.incident_proposed`, control told) or **refused** (403, audited
    `task.incident_refused` in its own transaction). An operator confirms or dismisses a proposal
    (`POST /spaces/{id}/tasks/{task}/incident`, `queue:write`, If-Match); confirming is itself a trigger check
    (`operator` must be trusted) and applies insertion and preemption then. Agents have no tool to confirm.
    Trusting `control` trusts its judgement: control acting on untrusted content can still call the tool without an
    inbox item; remove `control` from the list to make control relay only trusted sources.
  - **Worker handling** (`incident_workers`), for the assigned runs of each preempted task and of its subtasks, recorded
    per run in `task_preemptions` and audited (`task.preempted` with the runs and actions; `run.paused` /
    `run.stop_requested` per run): `finish` (default; the run keeps going and gets an idle-gated notice to wrap up and
    report; a preempted task may then go ready → review), `pause` (central queues the existing `run.interrupt` command:
    herdr-core sends Esc or Ctrl+C and the program stays, idle, with its session; skipped as `finish` while an operator
    has taken the terminal over) or `interrupt` (the run is stopped politely, session resumable). Control gets a
    durable notification listing the preempted tasks and what happened to their workers.
  - **Capacity.** `incident_capacity` (0-20, default 0) adds top-level slots only incident tasks may use, so an
    incident can start beside ordinary work without preempting it.
  - **Resumption** (`incident_resume`), evaluated in the transaction that ends an incident (done, failed or cancelled),
    never by polling: `manual` (default) tells control which preempted tasks wait; `auto` resumes every preempted ready
    task of the space in queue order: one that is eligible and still has a live worker runs again and its paused
    workers get an idle-gated "continue" notice; one whose workers are gone stays ready ("relaunch") for control to
    start a new worker; one that is not eligible yet keeps its preemption ("waiting") for a later resume. An explicit
    resume (`POST .../resume`, `queue:write`, or `spaceman_task_resume` for control) does the same for one task, and
    starting a preempted task by hand resumes its paused workers. Each resumption closes the preemption records with
    its outcome and is audited (`task.resumed`). Escalation is unchanged: it reorders only when the policy says so.
  - **`run:diagnose`.** A diagnostic launch needs `run:diagnose` in addition to `run:control`, checked after the space
    role decided the kind (a launch naming a diagnostic role is a diagnostic launch). Operators now hold
    `run:diagnose` (GOAL SM-CTRL-004 lets operators launch audited diagnostics), admins hold everything, viewers
    neither, so existing operators and admins keep working. Agents cannot launch diagnostics: control's worker tool
    launches workers only, and a space role cannot change its kind. Launches stay audited with their kind.
  - **Human-action type permissions.** `space_human_type_permissions` stores, per space and type (a built-in type or a
    custom type's ID), the weakest role that sees actions of the type (default viewer), asks with it through the API
    (default operator) and responds, assigns, comments, clarifies, moves or cancels (default operator), and which
    agents may ask (`any`, only listed space roles, or `none`). They only narrow the role permissions
    (`human_action:read`/`respond`, `approval:decide` still apply). Hidden actions are left out of the space and global
    lists and of the global pending counts; reading or acting on one is "not found", acting on a visible one without
    the responder role is forbidden. Changing them needs the new admin-only permission `human_type_permission:write`
    (authorization is admin business; operators read them with `space_config:read`), audited
    (`human_type.permission_set` / `_deleted` with before and after). Create without If-Match refuses an existing
    permission; update needs If-Match.
  - **User profile edits.** `PUT /users/{id}/profile` (global `user:write` with step-up, If-Match against the user's
    version) replaces email, display name and the administrator's MFA setting, audited `user.profile_updated` with
    the old and new email, the number of linked SSO identities and how the user was provisioned. Email is never an
    SSO key (identities are matched by issuer and subject, HP-OIDC-009), so a change never links, unlinks or merges an
    identity; an address in use is refused; a user with a password keeps an email (their sign-in name) and an email
    change ends that user's sessions (API tokens keep working). The users API now returns `mfa_required_set` (the
    administrator's setting) beside the effective `mfa_required`; clearing the setting never lifts MFA that policy
    forces (`auth.require_totp` in production, operator or admin grants on a password account). The last-administrator
    and SSO-only guards are checked relative to before the change.
  - **Terraform** (provider repository): `spaceman_human_action_type_permission`; `spaceman_user` updates email,
    display name and `mfa_required` in place; `spaceman_space_queue_policy` manages the incident fields.
- **Alternatives:** an incident lane or automatic incidents from events (the brief forbids an immutable lane; events
  stay inbox items for control); per-user type permissions (roles are the authorization model; a per-user list would
  bypass Casbin); keeping diagnostics on `run:control` (the separate permission existed precisely to separate them);
  treating `mfa_required` as the effective value on write (Terraform would see drift whenever policy forces MFA).
- **Consequences:**
  - **Security-relevant:** incidents from untrusted sources no longer preempt work; operators can hide or restrict
    human-action types; diagnostics need their own permission (operators keep it by default). Tests:
    `internal/queue` `TestIncidentTriggers`, `TestIncidentProposalDecision`, `TestPreemptedWorkers`,
    `TestIncidentResumption`, `TestIncidentCapacity`; `internal/agenttools` `TestIncidentTriggersFromTools`;
    `internal/authz` `TestDiagnoseAndTypePermissionRoles`, `TestHoldsRole`; `internal/human` `TestTypePermissions`;
    `internal/auth` `TestUpdateProfile`; `internal/app` `TestIncidentPolicyEndToEnd`,
    `TestOperatorRestartsControlAndDiagnostics`, `TestHumanTypePermissionsOverHTTP`, `TestUserProfileOverHTTP`.
  - Pausing is simulated: the in-memory runner exits on an interrupt; with a real harness Esc stops the turn
    (herdr-core contract) but no real-harness run has exercised a pause and resume.
  - The attention summary still counts pending actions of hidden types (a number per space, never their content); the
    change feed announces their IDs, which then read as not found.
  - A paused worker stays paused until resumed; nothing times it out.

## 2. Open items and unresolved contracts

| ID | Item | Blocks | Plan |
|---|---|---|---|
| K-1 | ~~hc framed protocol byte layout.~~ **Resolved 2026-11-02:** `ch u32 LE · kind u8 · len u32 LE`; DATA=0, CREDIT=1, CLOSE=2, RESIZE=3 (spike S-3). | — | done |
| K-2 | ~~Whether hc persists `env`.~~ **Resolved 2026-11-02:** yes, plaintext `restart_env` → ADR-0009 shim required. | — | done |
| K-3 | ~~hc parent semantics.~~ **Resolved 2026-11-02:** children killed when the parent program exits or is killed, unless `outlive_parent` → ADR-0017 lease guard. | — | done |
| K-4 | ~~Whether Pulse `durable` grants accept name patterns.~~ **Resolved 2026-11-02:** exact names only; DELETE permitted → fixed name + delete/recreate (ADR-0011). | — | done |
| K-5 | ~~ghostty-web package and API.~~ **Resolved 2026-11-02:** `ghostty-web` 0.4.0 (MIT): `init()`, `new Terminal({cols, rows, theme, fontSize, fontFamily, disableStdin, scrollback})`, `open`, `write(string \| Uint8Array)`, `resize`, `reset`, `dispose`; WebAssembly is inlined (ADR-0020). 2026-11-03 (ADR-0046): `term.options.disableStdin` is read on every key, `onData` delivers typed input and `focus()` exists (checked in the 0.4.0 dist). | — | done |
| K-6 | ~~Official Go MCP SDK version and streamable/stdio APIs.~~ **Resolved 2026-11-02:** `github.com/modelcontextprotocol/go-sdk` v1.8.0: `mcp.NewServer`, typed `mcp.AddTool`, raw `Server.AddTool` (input schema must be an object, or it panics), `IOTransport` for stdio. Upstream client (M3-03): `mcp.NewClient` + `Client.Connect` with `StreamableClientTransport{Endpoint, HTTPClient, MaxRetries, DisableStandaloneSSE, MaxEventSize}`; the connection context is detached from the `Connect` context; `ClientSession.Tools` (paginated), `CallTool`, `Close`. Transport failures surface as JSON-RPC code -32005. | — | done |
| K-7 | ~~TipTap extension set.~~ **Resolved 2026-11-02:** `@tiptap/react`, `starter-kit`, `markdown` (official Markdown parse/serialize), `extension-image`, `extension-list` (task lists), `extension-table`, `extension-file-handler` are MIT (npm registry). Pinned to 3.32.0 (released 2026-10-13); renderer `marked` 18.1.1 (MIT) + `dompurify` 3.4.17 (MPL-2.0 OR Apache-2.0). | — | done |
| K-8 | ~~llm-knowledge-base-template layout and licence; Piebald dream prompts licence.~~ **Resolved 2026-11-02:** KB template in `docs/contracts/kb-template.md`. Piebald-AI/claude-code-system-prompts is MIT, but its files are extracted Claude Code prompts whose text the repository licence may not cover; SpaceMan's dream prompt is written from the brief's principles and copies none of it (ADR-0027). | — | done |
| K-9 | Codex ChatGPT-login token materialization (`auth.json`) vs API key. | SM-ACCT-002 | Inspect Codex docs/behaviour; support API key first, document ChatGPT auth handling. |
| K-11 | ~~Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26).~~ **Resolved 2026-11-02:** SSO grants are stored under `user:<id>#sso`, which only SSO sessions act as (ADR-0029); SR-26 ported as `TestReviewSR26SSOGrantsOnlyForSSOSessions` (`internal/auth`, `internal/oidc`) and `TestOIDCSignInOverHTTP`. | — | done |
| K-10 | ~~Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode.~~ **Resolved 2026-11-02:** works interactively with an empty config dir once first-run state and folder trust are seeded (spike S-5, ADR-0021). | — | done |
