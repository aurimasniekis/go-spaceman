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
    concurrent upload of the same bytes is never lost. Only the local backend existed at first; the S3 backend followed
    with ADR-0048. Per-space quota: `artifacts.space_quota_bytes` (1 GiB); per-upload limit: `http.max_upload_bytes`.

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
    item → task → event) is not bounded yet; only an installation's own events are dropped. (ADR-0049 bounds it with
    `spaceman-depth`.)
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

### ADR-0048 — The artifact store: local directories or an S3 bucket behind one interface; backups copy S3 artifacts; Go interfaces for inbound sources and MCP transports (security-relevant change)
- **Status:** Accepted, 2026-11-03 (gap closure for SM-STORE-002 and SM-ARCH-003; amends ADR-0013's implementation
  notes, ADR-0028 and ADR-0031; no migration, no node protocol change).
- **Context:** `artifacts.backend: s3` passed validation but central refused to start with it: attachment content,
  central's copies of recordings and run transcripts were written straight to central's disk, and S3 existed only as a
  backup target (`internal/s3client`, ported from Hallpass). SM-ARCH-003 also asks for Go interfaces for artifact
  storage, inbound event sources and MCP transports; recordings and transcripts had none, inbound events were bound to
  the go-pulse SDK inside the ingestion manager, and the gateway built its streamable HTTP transport inline.
- **Decision — the store.**
  - `internal/artifacts.Store` is the artifact store: immutable objects under keys in three namespaces,
    `attachments/sha256/<aa>/<sha256>`, `recordings/<run>/seg-NNNNNN.smrec.zst` and `transcripts/<run>.jsonl`, with
    `Put` (size and SHA-256 known), `Open` (a seekable reader), `Stat`, `List` and `Delete`. Anything else under the
    prefix (temporary files, other tools' objects) is not an artifact and is never listed or written.
  - Two stores are compiled in. `Local` maps each namespace to a directory: attachments to `artifacts.local_dir`
    (default `<data>/artifacts`), recordings and transcripts to `<data>/recordings` and `<data>/transcripts`, which is
    the layout central already used, so the default backend needs no migration. `S3` keeps the same keys under
    `artifacts.s3.prefix` in one bucket through `internal/s3client`.
  - Writes never append to an object and never hold a whole file in memory. An attachment upload is streamed into a
    spool file in `<artifacts.local_dir>/tmp` while it is hashed and size-limited, then handed over (`PutFile`): the local
    store renames it into place, the S3 store uploads it in one PUT whose `x-amz-content-sha256` is the content's
    SHA-256, which the service verifies. Single PUTs bound an object to 5 GiB; attachments are bounded by
    `http.max_upload_bytes`, transcripts by 64 MiB and recording segments by 8 MiB, so no multipart upload is needed.
  - Reads stream. A seek turns the next read into a range GET from the new offset, so `http.ServeContent` answers
    `Range` requests for attachments and transcripts without downloading the object. A full sequential read of an
    attachment or a transcript is checked against its SHA-256 (`artifacts.Verified`): damaged content fails on the read
    that reaches the end, and those last bytes are withheld, so a download ends short instead of completing. Recording
    replay decodes every segment from its start and uses no ranges; zstd frame checksums cover segments.
  - Content addressing and deduplication are unchanged: an upload of content already stored is not uploaded again
    when the stored object is less than 30 minutes old, and is uploaded again (refreshing its modification time) when
    it is older. Garbage collection removes unreferenced content only when it is older than an hour, so it never
    removes content an upload has just found.
  - Quotas are unchanged: the attachment quota counts attachment rows in the database inside the upload transaction,
    whichever backend holds the bytes; the recording quota is the node's.
- **Decision — recordings and transcripts.**
  - Central's copy of a recording is spooled in `<data>/recordings/<run>/` while the node ships it (S3 has no append).
    With the S3 store it is archived when its close record arrives or its run ends: each segment is uploaded and then
    removed locally, in the background, outside any transaction. Reads, downloads and replays merge the spool and the
    bucket by segment name, so a recording reads the same before, during and after its archive. A node that re-ships
    its tail after the archive (it missed the acknowledgement) is deduplicated: the writer resumes from the last
    sequence of the last archived segment and numbers new segments after it. A failed archive leaves the spool;
    central archives the spooled recordings of ended runs at its next start.
  - A transcript is received into `<data>/transcripts/<run>.jsonl.part` as before and checked against its size and
    SHA-256; then it is handed to the store. If the bucket refuses it, the transcript is kept as `<run>.jsonl` on the
    local disk, recorded as stored, served from there, and uploaded at the next start. The node is never asked to
    send it again.
  - Deletes (an operator's, retention's and purge's) remove the objects as well as any local copy. Purging a space
    now deletes its runs' recordings and transcripts after the purge commits, with either store; before, a purge left
    them in the data directory.
- **Decision — credentials and isolation.** The bucket is configured like the backups' (`artifacts.s3`): keys come from
  files read at start, are never logged or put in an error, and `Location` shows only `s3://bucket/prefix`. TLS is always
  verified, a private-network endpoint needs `allow_private_network`, plain HTTP is development-only. The artifacts'
  prefix must not overlap `backups.s3.prefix` in the same bucket (configuration validation).
- **Decision — backups.** Backups still include every artifact, whatever the backend (SM-STORE-003 is not weakened):
  with the S3 store, every object is streamed from the bucket into the backup and sealed like a local file, in the
  same layout (`artifacts/`, `recordings/`, `transcripts/`), and attachment content is checked against its address on
  the way. A path the local spool already provided is not copied twice. The manifest records the store
  (`artifact_store: {kind, location, included: true, objects}`). There is no "reference only" mode: a backup that
  depended on the bucket surviving would not be a backup of the artifacts. A restore writes the artifacts to the local
  folders, as before.
- **Decision — migration.** Switching `artifacts.backend` moves nothing. The offline command
  `spaceman artifacts migrate --to s3|local [--remove-source] [--dry-run] [--json]` copies what the destination lacks
  (attachment content checked against its address), takes the data-directory lock (central must be stopped) and is safe
  to repeat. The procedure from local to S3: stop central, migrate to S3, set the backend, start central, then remove
  the local copies (`--remove-source` on a second run). After a restore into an installation with the S3 backend,
  `backup restore` prints the migrate command to run before starting central. Central with the S3 backend warns at
  start when attachment content is still on the local disk; it does not move it by itself.
- **Decision — interfaces (SM-ARCH-003).** Narrow interfaces, compiled in, with the existing implementations behind
  them and nothing new added:
  - artifact storage: `artifacts.Store` (`Local`, `S3`), with `attachments.Blobs` (content addressing) on top of it;
    `fleet.Options.RecordingStore` and `usage.New` take the store;
  - inbound event sources: `events.Source` (`Delivery`, `Start`), one per delivery mode; the ingestion manager picks
    the source by the subscription's delivery mode. The durable (JetStream through go-pulse) and core NATS sources are
    the existing code. A delivery mode without a source is reported on the subscription;
  - MCP upstream transports: `gateway.Transport` (`Name`, `Client`); `StreamableHTTP` is the only one and the default.
- **Security-relevant change.** No default changes and no existing guarantee is relaxed, but recordings, transcripts
  and attachments can now leave central's disk. Objects are stored as they are on the local disk, without client-side
  encryption, so the bucket must be private to SpaceMan, with server-side encryption if wanted; its credentials can
  read every artifact. One prefix belongs to one installation: attachment garbage collection deletes unreferenced
  content under it, so two installations (or a restored copy) sharing a prefix would delete each other's content.
- **Alternatives:** multipart or SigV4 streaming (chunked) uploads to avoid the spool (more protocol surface, and the
  spool is bounded and on central's own disk); an unsigned payload (gives up the service's integrity check); keeping
  recordings only on the local disk (does not meet SM-STORE-002); backups that only reference the bucket (rejected
  above); migrating automatically at start (moves data without the operator asking, possibly for hours).
- **Consequences:** central still needs local disk for the spool: live recordings, transcripts being received and
  uploads in progress. A backup of a large bucket copies all of it, which takes time and space in the backup target.
  Objects of runs that are not in a restored database stay in the bucket (attachment content is collected; recordings
  and transcripts are not). The transcript lock is held during the upload of a final chunk (at most 64 MiB). Resuming
  an archived recording reads its last segment from the bucket under the recordings lock. Evidence: the in-process
  fake bucket (`s3test`, which now serves ranges and modification times) and the opt-in MinIO suite
  (`TestRealS3Artifacts`); AWS S3 and R2 are unverified.

### ADR-0049 — Paused-space event policies, bounded automation depth across installations, routed responses as inbox items (security-relevant change)
- **Status:** Accepted, 2026-11-03 (gap closure: SM-EVT-014, SM-EVT-016, SM-INBOX-001).
- **Context:** Three requirements were partial. SM-EVT-014: a paused space always ingested, and with **Hold** a control
  run still running was told about new event items at once, so work was not deferred; GOAL asks that paused spaces keep
  ingesting by default but defer wakes and work according to policy. SM-EVT-016: the loop guard (ADR-0030) dropped only
  an installation's own events, so a loop through another installation (A publishes, B ingests and publishes, A ingests)
  was unbounded. SM-INBOX-001 lists routed human responses among the inbox's inputs, but the `human_response` source
  kind had no producer: routed responses were notifications only (ADR-0032). Putting responses into the inbox changes
  who can see what (every run of a space reads the inbox), and the depth bound drops inbound events, so this is recorded
  as a **security-relevant change**.
- **Decision:**
  - **Paused-space policy per subscription.** Migration `0035` adds `event_subscriptions.while_paused`: `defer`
    (default), `wake` or `stop`, in the API (`while_paused` on create and update; omitted on update keeps it), the CLI
    (`--while-paused`) and the UI (subscribe dialog and subscription page). A per-subscription setting was chosen over a
    per-space one because the right answer differs by source (an incident feed should wake a held control; a backlog
    feed should wait), and subscriptions already carry the error and consumer policy (SM-EVT-005).
    - `defer`: the event becomes an item as usual; whether its control notice is deferred is decided in the item's own
      transaction from the space's state at that moment (`inbox_items.notice_deferred`), so a pause or resume racing an
      ingestion cannot lose the notice. The supervisor does not notify control about a deferred item. When an operator
      resumes the space (fleet `OnSpaceState`), the supervisor sends each still-`new` item's ordinary control notice
      (the same message the notifier delivers for any new item, deduplicated per item) and then clears the flag, oldest
      first; a crash between the two sends it once more, never zero times. Clearing the flag changes no version.
      Central's start sends the notices of active spaces that a resume did not finish. Nothing polls.
    - `wake`: the item's notice goes to control at once (the behaviour before this ADR).
    - `stop`: the ingestion manager stops the subscription while its space is paused (state `paused`; the manager is
      woken by the pause and the resume). A durable consumer keeps its position on the server (go-pulse v0.1.3
      `Subscription.Stop`), so its events are ingested after the resume; a core NATS subscription receives nothing
      meanwhile. Events delivered before the stop took effect are treated as `defer`.
    - Launches stay refused while paused (SM-SPACE-003), so "work" is deferred by not telling control; other inputs of a
      paused space (operator items, messages, schedules) are not covered.
  - **Automation depth (`spaceman-depth`).** A new SpaceMan extension header, beside `spaceman-origin`,
    `spaceman-causation-id` and `spaceman-correlation-id` (ADR-0030); no Pulse envelope field is used or invented.
    - An item made from an inbound event gets depth = the event's `spaceman-depth` + 1 (no header: 0, so an event from a
      non-SpaceMan source makes a depth-1 item); `inbox_items.depth`. A task created from an inbox item
      (`origin_kind` `inbox_item`) or under a parent gets the deeper of their depths (`tasks.depth`). The outbox stores
      the depth of each event (`event_outbox.depth`) from the item or task it reports, and the relay sends
      `spaceman-depth` when it is above 0.
    - The inbound side checks the depth after the definition accepted the event (so filtered events are not
      quarantined for depth): at or over the space's limit `event_depth` (limits model, ADR-0036; default 8, 1–64,
      `limits.event_depth`, space override on the Limits page, API and CLI), or a header that is not a plain decimal
      number of at most 1,000,000, the event is **quarantined** with the reason and terminated (never redelivered), and
      its depth is kept (`event_quarantine.depth`). Quarantine rather than a silent drop: the loop is visible (the
      quarantine count, the Attention page) and an operator can **reprocess** it, which ingests it without the depth
      check at depth + 1, so the next installation stops it again.
    - The depth follows only links SpaceMan records. Runs, human actions, approvals, memory and KB events carry none:
      a change control makes on its own judgement after reading an item starts a new chain, as a person's would, and is
      bounded by the run, tool-call and budget limits. Pulse does not authenticate the header; a non-SpaceMan publisher
      can send any value, which only affects its own events.
  - **Routed responses as inbox items (decision for SM-INBOX-001).** The kind is kept and given a producer, not
    removed: GOAL lists routed responses among the inbox's inputs, and an answer whose asker is gone is exactly the
    kind of input control should triage (reply, task, defer, associate) instead of only acknowledging. Migration `0035`
    adds `human_policies.response_inbox`: `off` (default and built-in), `routed` (a response routed to control or the
    operators because the requester's run ended, or sent to control by a person) or `always` (every answered action of
    the type, whoever asked: an agent, an operator or SpaceMan). In the API (`response_inbox` in the policy fields;
    omitted is `off`), the CLI (`--response-inbox`) and the policy dialog.
    - The item is created in the routing (or finalizing) transaction through `inbox.CreateTx`, with source kind
      `human_response`, source ID the action, the action's title (as `Response: <title>`) and priority, correlation the
      action's, and dedup key `response:<action>`: at most one item per action however often it is routed. Only answers
      (`resolved`) make items; withdrawals, expiries and clarification requests stay notifications.
    - **The item never contains the response.** Its body and trusted metadata name the action, type, outcome, who
      answered, the requester and the routing (target, reason, notification); the body tells control to read the
      response with `spaceman_human_get`, which keeps the ADR-0032 rule that only related runs and control see
      responses. Every run of a space can read the inbox, so copying the response would widen who sees it.
    - A response routed to control already reaches control as its notification, so the supervisor does not send a
      second "new inbox item" notice for that item.
  - Node protocol: unchanged (no minor bump).
- **Alternatives:** a per-space paused policy (one answer for every source); deferring every control notice of a paused
  space (also hides operators' own requests from a held control, which they may want it to see); polling paused spaces
  for resumption (forbidden, SM-ARCH-004). A hop list of origins instead of a depth counter (detects exact cycles but
  grows without bound and leaks installation names into every event); dropping over-deep events as `filtered` (no
  inspection or override). Removing `human_response` with a migration (drops a GOAL input); copying the response into
  the item (discloses it to every run of the space).
- **Consequences:**
  - New subscriptions defer by default: a control run kept running under Hold no longer hears of event items until the
    space resumes, unless the subscription says `wake`. Existing subscriptions get `defer` too.
  - An event loop between installations ends after `event_depth` ingestions in quarantine; legitimate chains longer
    than the limit need a higher limit or a reprocess.
  - With `response_inbox` set, control sees an inbox item and, for responses routed to control, its notification;
    operators see items for undeliverable responses in the inbox.
  - The Terraform provider (separate repository, ADR-0047) manages both: `while_paused` on
    `spaceman_event_subscription` and `response_inbox` on `spaceman_human_action_policy` (provider `e52e68c`).
  - Evidence is simulated: the in-process Pulse-shaped server with the real go-pulse SDK, including two centrals in
    one test; no real pulsed and no real harness ran these paths.

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

### ADR-0051 — Starter roles: example profiles, prompts and role templates added once, given to spaces on request
- **Status:** Accepted, 2026-11-03 (operator request: a new installation should be usable without configuring every
  role by hand; SM-PROMPT-005; no migration, no node protocol change).
- **Context:** A space launches a configured role only after an operator has created a prompt, a harness profile, a
  role template and a space role. A first-time user had to do all of that before the first useful run, and KB sessions
  and dreams also need space roles of kind `kb` and `dreamer`. Role templates and profiles could not be deleted.
- **Decision:**
  - **Starter set, added once.** On the first start, central adds two harness profiles (`starter-claude-code` and
    `starter-codex`, the harnesses' own defaults), a published prompt for each starter role (`starter-<role>`) and six
    role templates: `starter-control` (control), `starter-planner`, `starter-implementor`, `starter-reviewer` (worker),
    `starter-kb` (kb) and `starter-dreamer` (dreamer, with the built-in `dream-consolidation` prompt). The templates
    use the Claude Code profile. An entry whose slug exists is left alone. When the set is complete, the setting
    `catalog.starter_roles_seeded` records it, and later starts add nothing, so an operator's edits and deletions are
    final.
  - **Ordinary entries.** They are edited, versioned and deleted like any other profile, prompt or template. The
    `starter-` slugs keep them apart from an installation's own entries.
  - **Spaces get them on request.** `POST /spaces/{id}/starter-roles` (`role add-starters`, **Add starter roles** on the
    space page) adds a space role named after each starter role (control, planner, implementor, reviewer, kb, dreamer)
    whose template still exists and whose name the space does not use. Existing roles are never changed. Creating a
    space with `starter_roles: true` (`--starter-roles`, a checkbox the create dialog ticks by default) does the same.
    A failure there is logged; the space is created either way. Spaces created through the API without the option get
    no roles, as before.
  - **Deleting templates and profiles.** `DELETE /role-templates/{id or slug}` and `DELETE /harness-profiles/{id or
    slug}` delete an entry nothing uses, and are refused while a space role (or, for a profile, a template) refers to
    it, naming up to five users. Runs already launched keep their manifests.
- **Consequences:**
  - A new installation can create a space and launch control, planners, implementors and reviewers at once.
  - The prompts give guidance only: they name tools the role's kind already has and grant nothing (SM-SEC-001).
  - Codex users switch the templates' profile to `starter-codex` (one edit per template) or launch with a space-role
    profile override.
  - Installations that existed before this change get the starter set at their next start (the setting is new).

### ADR-0052 — Notices to finished turns, a sensible terminal size, and a web terminal that survives reopening (security-relevant change)
- **Status:** Accepted, 2026-11-03 (operator reports from a dev stack running real Claude Code: a second inbox item
  never reached control; the run page hung the browser tab when reopened; the terminal was far taller than the page;
  no node protocol change, no migration).
- **Context:**
  - herdr-core's idle gate takes one exact status (`if: idle|working`, hc 0.1.16). Claude Code reports `done` after
    every turn and stays there, so every notice after control's first turn was refused, and the refused notice was
    then held for the 5-minute retry meant for notices that were typed (S-6).
  - ghostty-web 0.4.0: disposing a terminal that drew Claude Code's full-screen start-up (alternate screen, mouse and
    keyboard modes, synchronized output, a path in its banner) leaves the shared WebAssembly module in a state where
    the next terminal's resize never returns; the tab hangs (Chrome `RESULT_CODE_HUNG`). Leaving the run page and
    coming back inside the app does exactly that; a page load starts a fresh module, so it was fine.
  - herdr-core starts programs at 200×120 when nobody is attached; agents lay out for 120 rows, about two screens of a
    browser, and a watch-only viewer never resizes a program. ghostty-web cancels wheel events over the terminal, so
    the box could not be scrolled with the wheel, and the view stayed at the top while the program drew lower down.
  - Over HTTP/1.1 a browser opens at most six connections per host for all its tabs; each tab held two streams (the
    change feed and a live terminal), so a few tabs exhausted them (reproduced: a fourth tab could not load).
- **Decision:**
  - **Notices reach finished turns.** `Runner.Send` with the idle gate sends `if: idle`; when herdr-core refuses and
    `terminal.get` still reports `done`, it sends ungated. A `done` program has finished its turn and waits for input,
    as an idle one does; a working, blocked or erroring program is still refused. **Security-relevant:** between the
    status check and the send the program could start working (only through other input); herdr-core still waits
    ~3 s without human typing before pressing Enter, and a notice carries no content (it only says to call
    `spaceman_messages`). A refused notice is due again at once, so the run's next idle report retries it.
  - **Terminal size.** The agent writes `headless = "<runner.terminal_size>"` (default `160x48`, validated
    40-400×10-200) into its core's `config.toml`; programs started afterwards use it, with no core restart. Running
    programs keep their size.
  - **Web terminal.** A terminal is reset (RIS, `ESC c`) before it is disposed; `Terminal.reset()` does not avoid the
    hang. The view follows the cursor's row as output arrives (until the viewer scrolls away), and the wheel scrolls
    the box before it reaches ghostty-web's scrollback.
  - **Connections.** One change-feed stream serves all tabs of a browser: the tab holding the Web Lock
    `spaceman-watch` opens it and relays it over a BroadcastChannel; another tab takes over when it closes. Without
    Web Locks (not a secure context: plain HTTP to another host) each tab keeps its own stream. A tab hidden for 5 s
    releases its live terminal and resumes from the last record it drew when shown again.
- **Consequences:**
  - Control and workers get notices after every turn, as SM-COMM-003 intended. The fake runner always behaved this
    way, which is why the tests did not catch it; `TestRealSendIdleOrDone` now runs against real herdr-core.
  - `make e2e` reopens a full-screen run terminal (the stub harness's `/fullscreen` prints the start-up with neutral
    text); without the reset the journey hangs.
  - Existing runs keep 200×120 until relaunched; an agent restart applies the new size to later launches.
  - ghostty-web's `onRender` is declared but never fired in 0.4.0; the view follows `onCursorMove` and resizes.

### ADR-0053 — Codex ChatGPT logins as one live file per account on the node, and Codex skills (security-relevant change)
- **Status:** Accepted, 2026-11-03 (closes SM-ACCT-002 and SM-MCP-004; migration 0036, no node protocol change).
- **Context:** Codex runs used only an OpenAI API key (K-9), and got no skills because Codex's skill loading was
  unverified. A spike against codex-cli 0.166.0, with an isolated `CODEX_HOME` and no credential, settled both
  (docs/contracts/harnesses.md): skills in `$CODEX_HOME/skills/<name>/SKILL.md` reach the model's prompt input; logins
  are stored in `$CODEX_HOME/auth.json` by default; a symlinked `auth.json` is rewritten through the link; and a
  ChatGPT login's refresh token works only once ("your refresh token was already used").
- **Decision:**
  - **Skills.** The Codex adapter materializes skills into `$CODEX_HOME/skills`, like Claude Code's, and advertises
    `skills.files`. Codex's own `.system` skills are left alone (they carry no SpaceMan marker).
  - **ChatGPT login kind.** `codex_chatgpt_login` holds the `auth.json` `codex login` writes: a JSON object with
    `auth_mode: chatgpt` and a non-empty `tokens` object, at most 64 KiB, stored compacted and sealed like every
    secret (migration 0036 rebuilds `accounts` to allow the kind). It reaches a launch under the payload key
    `CODEX_AUTH_JSON`, never as an environment variable and never in the launcher's secret file.
  - **One live copy per account and node.** The node keeps the login in `<data>/logins/<sha256(account)[:32]>/auth.json`
    (0600, folder 0700) with `auth.lock` beside it, and links every instance's `$CODEX_HOME/auth.json` and `auth.lock`
    there, so refreshes by any run are what the next run reads, and concurrent refreshes share one lock. The seed from
    central replaces the live copy only when it differs from the seed the copy started from (`seed.sha256`): a
    rotation applies, a refresh is never undone. An instance launched with an API key loses links left by a login; a
    regular `auth.json` of its own is kept.
- **Consequences:**
  - **Security-relevant:** unlike other credentials, a ChatGPT login stays on the node's disk between runs (the agent's
    user and root can read it), and it is not removed when the account is deleted in SpaceMan.
  - One ChatGPT login should be used on one node: two nodes refresh separate copies, and the first refresh breaks the
    other (single-use refresh tokens). This is documented, not enforced.
  - A real ChatGPT login and a real Codex turn remain unverified (no account or credential); the layout is verified
    with an API-key file (`TestRealCodexReadsNodeLayout`, codex-cli 0.166.0).

### ADR-0054 — An OpenAI API key reaches Codex as a stored login in its CODEX_HOME (security-relevant change)
- **Status:** Accepted, 2026-11-04 (found by a dry run of the real Codex suite through SpaceMan with codex-cli 0.166.0;
  no migration, no node protocol change).
- **Context:** SpaceMan handed an `openai_api_key` account to Codex as `OPENAI_API_KEY` through the launcher (ADR-0009),
  as for Claude Code. The interactive Codex 0.166.0 ignores that variable, and `CODEX_API_KEY` too, even with
  `forced_login_method="api"`: it stops at its first-run screen ("Choose how you want to use Codex") and reports no
  status, so such a run never starts working. With `$CODEX_HOME/auth.json` in API-key mode (what
  `codex login --with-api-key` writes) it starts at once, sends the prompt and reports its status through OSC 7501.
  Only fakes had run Codex before, so nothing had shown this.
- **Decision:** For an API key, the Codex adapter also returns a credential file:
  `$CODEX_HOME/auth.json` = `{"auth_mode":"apikey","OPENAI_API_KEY":…}`. The node writes it with mode 0600 through a
  temporary file and a rename, after removing links left by a ChatGPT login (ADR-0053), so it replaces a link and never
  writes through one. It is rewritten at every launch, so a rotation applies at once. The key still also goes to the
  launcher as before.
- **Consequences:**
  - **Security-relevant:** an OpenAI API key bound to a space stays on the node's disk between runs, in each instance's
    `CODEX_HOME` (0600, readable by the agent's user and root), and is not removed when the account is deleted.
    ADR-0009's "secrets only through the launcher's file" no longer holds for Codex API keys; Claude Code credentials
    are unchanged.
  - Codex's OSC 7501 status is now verified live (`status {app: codex, state: working}` in herdr-core), so terminal
    notices work for Codex runs.
  - The real-run suite (`make test-real-codex`) needs only a key or a ChatGPT login to verify a full turn.

### ADR-0055 — Release images are published to Docker Hub
- **Status:** Accepted, 2026-11-04 (operator request; amends the publishing part of ADR-0034, which pushed no image).
- **Context:** The release workflow built and smoke-tested both images but published none, so evaluators had to build
  them. The repository now has a `DOCKERHUB_TOKEN` secret, as the sibling projects do (Porch publishes
  `docker.io/aurimasniekis/porch` the same way).
- **Decision:** For a `v*` tag, the release workflow pushes `docker.io/aurimasniekis/spaceman` and
  `docker.io/aurimasniekis/spaceman-agent` for linux/amd64 and linux/arm64, built from the same Dockerfiles and build
  arguments as the smoke-tested images, tagged with the version (and `latest` for a final release only), with SBOM and
  `mode=max` provenance, and a GitHub build provenance attestation of each digest. Without the secret the release goes
  ahead without images and says so. The draft release notes list the pushed digests.
- **Consequences:** Images appear on Docker Hub when the tag is pushed, before the draft GitHub release is published
  by hand. What the images contain is unchanged (ADR-0034): no herdr-core and no harness, and the agent image is not
  agent-ready by itself. Images are not scanned in the release job; CI's security job runs govulncheck on the code.

### ADR-0056 — Control starts for waiting work and stops when idle
- **Status:** Accepted, 2026-11-04 (operator decision: on by default; stop after an idle timeout).
- **Context:** Control only woke if a control run was already live. An operator had to launch control by hand, and an
  idle control kept its harness session running for as long as nobody stopped it. Events still became durable
  notifications to control (SM-CTRL-002), but they waited until someone started it.
- **Decision:**
  - **Auto-start.** The notifier's sweep lists the active spaces where something waits for control that no control has
    seen and no control run is authoritative: a stored message to control, or a pending human response that wakes it.
    The sweep runs on every new message and once a minute. For each such space the supervisor launches control as the
    system actor `control-autostart`, unless:
    - the space turned auto-start off;
    - its node holds no valid lease;
    - it already started control six times in the last hour (it then raises a human action).

    The launch resumes the newest control run: role, harness, instance, model, settings, account and workspace. It
    resumes that run's session only when the run finished a turn, because a session with no turn was never saved. A
    space that never had a control run gets its control role: the space role named `control` when its template is of
    kind control, otherwise the first such role by name. A refused launch (account, budget, relocation, validation)
    raises a high-priority human action, "Control could not start", deduplicated per hour. The unique index on one
    authoritative control run per space settles a race between two starts.
  - **Idle stop.** When a control run reports idle or done, the supervisor arms a timer. When the timer fires, it stops
    the run (interrupt, then kill after the grace) as `control-idle`, but only if the run is still idle at the same
    turn. If an operator holds a takeover, or a stored message or unwoken human response waits for the run, the timer
    starts over instead. The stop sets desired=stopped, so recovery does not restart the run; the next waiting work
    auto-starts it. Timers live in memory. Serve re-arms them for idle control runs at start, each with its full idle
    time.
  - **Settings.** Both are space limits in the one limits model (ADR-0036): `control_autostart`, a new unit `switch`
    (0 off, 1 on, default on), and `control_idle_stop_minutes` (0: never, default 30, at most 10,080). This gives them
    installation defaults, per-space overrides, audit, the API, the Limits page and Terraform without a new resource.
    The in-process test environment defaults both to off so run counts stay predictable. `TestControlAutostart` turns
    them on.
  - Control recovery after a crash (bounded restarts) now also resumes a session only when it finished a turn.
- **Consequences:**
  - Control costs harness time only while there is work. Each start after an idle stop is a new run, with a new
    recording and a new run token.
  - An item a control saw (noticed or fetched) but left unacknowledged does not start control on its own. The next
    control is told about it when it first becomes idle.
  - An idle stop interrupts a program that is waiting at its prompt. Work in the middle of a turn is never stopped.
  - Not security-relevant: the launch goes through the same resolution, admission (limits and budgets) and audit as
    any launch, as a system actor.

### ADR-0057 — Repository SSH keys held by SpaceMan (security-relevant)
- **Status:** Accepted, 2026-11-05 (operator decision; amends ADR-0044, which offered no SpaceMan-held Git credentials).
- **Context:** On the first deployment, clones of private repositories failed: the node agent's user had no SSH key,
  so git failed with "Host key verification failed". Giving that user a key would expose it to every agent, because
  agents run as the same Unix user as the node agent (SM-INV-007). The operator wants SpaceMan to clone private
  repositories without agents being able to take the credential. Options considered:
  - a HallPass route for the node: its key would still sit in the node user's home;
  - a GitHub App minting short-lived tokens: GitHub only, and it needs an App per installation;
  - SSH deploy keys held by SpaceMan: chosen. They work with any Git host, and the key never lands on the node.
- **Decision:**
  - **Key and storage.** A repository may have one SpaceMan SSH key (Ed25519), generated by central on request
    (`POST /spaces/{id}/repositories/{id}/ssh-key`; again to rotate; `DELETE` to remove; `space_config:write`, audited
    with the fingerprint). The private key is sealed with the keyring's credential purpose, with the repository ID as
    associated data. The API, the change feed, audit, exports and logs carry only the public key and its SHA256
    fingerprint.
  - **Delivery.** The key reaches the node only on the wire message of a `space.repos.sync` or a launch that names the
    repository. The hub attaches it at dispatch time, as it does run secrets, and only on sessions that negotiated
    protocol minor 7. Stored command payloads never contain it.
  - **Use on the node.** The node never writes the key to disk, the environment or its journal. For each clone,
    fetch or `ls-remote` it loads the key into an in-memory `ssh-agent` keyring and serves it on a socket in a fresh 0700
    folder that is removed afterwards; open connections are cut. Git runs with this ssh command:
    `GIT_SSH_COMMAND=ssh -F /dev/null -o IdentityAgent=<socket> -o UserKnownHostsFile=<pins> -o GlobalKnownHostsFile=/dev/null -o StrictHostKeyChecking=yes -o BatchMode=yes`.
    So no node SSH configuration applies, and only pinned host keys are accepted.
  - **Host-key pins.** The built-in pins are GitHub's published keys (from api.github.com/meta, with their fingerprints
    checked). Other hosts need the repository's `known_hosts` (validated: at most 32 entries, 16 KiB).
  - **URLs.** A key applies only to SSH URLs: generating one for an https repository is refused, as is changing the
    URL of a keyed repository to a non-SSH one. Repositories without a key keep the node's own Git authentication
    (ADR-0044, unchanged).
- **Consequences:**
  - Agents never receive the key. While an operation runs, an agent on the same Unix user could connect to the
    socket and have it sign: it cannot read the key, and with a read-only deploy key it gains read access to a
    repository it already has checked out. Full isolation needs agents under another Unix user (not done).
  - The key is long-lived until rotated, and its strength rests on central's keyring and KEK (backups contain it
    sealed).
  - Agents' own git commands (pushes, fetches in their worktrees) still use the node's own authentication.
  - Relocation does not pre-clone keyed repositories on the destination; the first sync after the move does.
  - Agents older than minor 7 ignore keys, and the repository shows a note.
  - Terraform manages `known_hosts` and generates or rotates the key, exposing the public key for a host's
    deploy-key resource.

### ADR-0058 — Shared Git SSH keys (security-relevant)
- **Status:** Accepted, 2026-11-06 (operator request; extends ADR-0057).
- **Context:** A repository's own key (ADR-0057) is a GitHub deploy key, and GitHub allows a deploy key on one
  repository only. An operator with many repositories wants one key, added once to a Git account, that all their spaces
  can clone with.
- **Decision:**
  - **Keys.** Administrators (`account:write`, like harness accounts) generate shared Git SSH keys: Ed25519, with a
    unique name and a description. The private half is sealed with the keyring's credential purpose, with the key ID as
    associated data. The API, CLI and UI show only the public half and its fingerprint. A key can be renamed, rotated
    (the old pair stops working at once) and deleted. Deletion is refused while a repository uses the key.
  - **Scoping.** Each key lists the spaces that may use it, or `all_spaces`. A space's operators see only the keys their
    space may use (`GET /spaces/{id}/git-ssh-keys`). A space can't be taken off a key's list while one of its
    repositories uses the key.
  - **Repositories.** A repository chooses a shared key with `git_key_id`, an SSH URL only. It has its own key or a
    shared key, never both.
  - **Delivery.** The repository service's `Key` resolves the repository's own key, else its shared key while the space
    may still use it. The key then travels exactly as in ADR-0057: on the wire message of a clone or fetch only, served
    from an in-memory agent with pinned host keys. If a space is no longer allowed to use the key, the repository shows a
    note, and the node falls back to its own Git authentication.
- **Consequences:**
  - One key on a person's account can push to every repository that person can reach. The UI and docs recommend a
    read-only machine user.
  - The exposure while a fetch runs is the same as in ADR-0057: an agent on the node's Unix user could ask the agent
    socket to sign, but can never read the key. A shared key reaches more repositories, though, so the same signing
    window could read any repository its account can.
  - Relocation leaves repositories that use a shared key to the first sync after the move, as it does for repositories
    with their own key.
  - Terraform manages keys as `spaceman_git_ssh_key`, with `public_key` exposed for the account's SSH key resource, and
    repositories reference them with `git_key_id`.

### ADR-0059 — Claude Code settings on harness profiles (security-relevant)
- **Status:** Accepted, 2026-11-06 (operator request: full control of a run's settings.json).
- **Context:** SpaceMan writes only `.claude.json` into a run's `CLAUDE_CONFIG_DIR`, so runs get Claude Code's built-in
  settings. As a result, operators couldn't tell auto mode what to trust, give permission rules, or add hooks. An
  operator whose agents reach Git through a gateway host had auto mode refuse ordinary work (pointing a remote at the
  gateway). Node-wide managed settings would change every Claude Code session on the machine, not only SpaceMan's runs.
- **Decision:**
  - **Model.** A claude-code harness profile has `claude_settings`: a JSON object of settings.json keys, any keys
    (`permissions`, `autoMode`, `env`, `hooks`, `statusLine`, `model`, …). A codex profile can't have them. Saved
    objects are at most 256 KiB. Only the keys SpaceMan edits are checked for shape:
    - `permissions.allow`, `ask`, `deny` and `additionalDirectories` must be string lists;
    - `permissions.defaultMode` must be a known mode;
    - `autoMode.environment`, `allow` and `soft_deny` must be string lists;
    - `env` values must be strings.

    Central checks them on save and the node again before writing.
  - **Ceiling.** `permissions.defaultMode` above central's `harness.max_permission_mode` is lowered to it when a run is
    resolved, like the `permission_mode` setting, and the effective configuration says the policy decided. The stored
    profile keeps what was written.
  - **Resolution.** The settings come from the role's profile only; templates, space roles and launches don't layer them.
    They are recorded in the run manifest after policy, shown in a role's preview, and apply to later launches.
  - **Delivery.** The launch payload carries them (`claude_settings`, protocol minor 8). At every launch the node sets
    each of the profile's top-level keys in the run's `$CLAUDE_CONFIG_DIR/settings.json`. It removes keys it set at an
    earlier launch that the profile no longer has (recorded in `settings.json.spaceman-keys`), and keeps keys Claude Code
    wrote there itself.
  - **Old nodes.** Central fails a run with settings as `node_too_old` instead of dispatching it to a node below
    minor 8, which would ignore them and drop what they add, deny rules included.
  - **UI.** The profile form edits permissions (default mode, allow, ask, deny, additional directories), auto mode
    rules (with a switch for Claude Code's defaults instead of a typed `"$defaults"`) and environment variables, and
    every other key as JSON.
- **Consequences:**
  - Whoever can edit harness profiles (`prompt:write`) controls the profile's runs' Claude Code settings, hooks
    included. Hooks and status lines run commands on the node as the agent's user, as runs themselves already can.
    Profile changes are audited (`profile.created` and `profile.updated` record the changed top-level settings keys
    and a sha256 of the new settings, never their values), and the settings are in every run's manifest.
  - **Accepted risk (operator decision, 2026-11-07):** settings may set `env` (including endpoint and credential
    variables such as `ANTHROPIC_BASE_URL`), credential helpers (`apiKeyHelper` and the like) and hooks, and only
    `permissions.defaultMode` is held to the permission mode ceiling. Profile editors (admins) are trusted with run
    credentials and node command execution (THREAT_MODEL T-32).
  - Settings are stored in clear and recorded in manifests, so they must not hold secrets; accounts carry credentials.
  - A key a run writes into its own settings.json carries over to the next launch unless the profile sets or once set
    that key. The node records the keys it owns before writing settings.json, so an interrupted write never leaves a key
    it set behind. A corrupt record can't say which keys those are: the node then moves settings.json and the record
    aside (`.corrupt-<unix nanoseconds>`), logs a warning naming both, and starts from fresh settings, so no stale key
    survives and the instance recovers without a fix on the node (Claude Code's own keys in that file are set aside
    too).
  - Terraform manages them as `claude_settings` on `spaceman_harness_profile`, a JSON string (`jsonencode`). An API
    update that leaves `claude_settings` out keeps the stored settings, so older clients don't wipe them.

### ADR-0060 — Harbor Pilot gateway: one central connection, spaces opt in per harness, per-run child tokens (security-relevant change)
- **Status:** **Proposed**, 2026-11-06. Not yet accepted. The operator answered design questions Q1–Q7 on 2026-11-06
  (listed at the end). The points they settle are marked **(Qn)**. Q4 stays **open**, with an interim default that
  control proposed. Implemented so far (HP-T2): the connection and space settings (decision 1; decision 2 for
  the stored tokens; decision 3's central-side rules) and the version probe of decision 8. Runs don't use them until
  HP-T3/T4. Contract:
  `docs/contracts/harbor-pilot.md` (v0.1.3); live spike: `docs/contracts/harnesses.md` (Harbor Pilot gateway spike, and
  the 0.1.3 re-run).
  - **Updated 2026-11-07 for Harbor Pilot 0.1.2/0.1.3**, which answered SpaceMan's upstream requests (Q5): per-run usage
    by trusted ref, budget refusals as `access.request.blocked`, `system`-role messages (K-13 resolved), the minting
    credential as `created_by`, and an optional budget check on mint. **The minimum Harbor Pilot version is 0.1.3**
    (decision 8).
- **Context:**
  - **What operators want.** Run model traffic should go through Harbor Pilot (HP), a self-hosted LLM gateway. HP gives
    failover, hard per-project spend limits and usage for every harness.
  - **How HP issues tokens.** It mints short-lived child tokens from a parent token that holds `tokens:mint`. Each child
    carries trusted `bound_refs`, which HP records on every request.
  - **SpaceMan today.** Accounts are per-harness secrets that reach the run unchanged (`accounts.SecretEnv`).
  - **The spike showed both harnesses work with only a secret env.**
    - Claude Code 2.1.317 runs with `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` and the ADR-0021 seed.
    - Interactive codex-cli 0.166.0 runs with a custom `model_provider` whose `env_key` is `HARBOR_PILOT_TOKEN`,
      without a login screen or `auth.json`.
    - HP's request detail shows `trusted_refs.spaceman.run` for both.
- **Decision:**
  1. **A central integration, not an account kind.**
     - **One connection per central.** It is a singleton `harbor_pilot_connection` row and holds:
       - the base URL;
       - the **parent token** (`hpk_`, scopes `gateway:invoke`, `gateway:models`, `tokens:mint`);
       - an optional **service-principal token** (`hpa_`, `usage.read` + `config.read` for budget status and per-run
         usage, plus `activity.read` for request activity and Harbor Pilot's events API).
     - **Each space opts in** through `space_harbor_pilot`, which has these fields:
       - `enabled`;
       - `project_id` (required when enabled);
       - `harnesses`: the list of harnesses routed through HP **(Q6)**. It is open to any harness HP can serve, not
         only Claude Code and Codex. A harness not in the list keeps its direct accounts;
       - `ttl_seconds`: default 86400 (24 h), min 900 (15 min), max 604800 (7 days) **(Q2)**;
       - `on_budget_status_unavailable`: `proceed` (default) or `refuse` **(Q3)**;
       - `allowed_model_aliases`: optional.
     - **Why not an account kind:**
       - an account's secret is, by construction, the value delivered to the run, so one bug would hand `tokens:mint`
         to an agent;
       - a kind carries one harness;
       - project, TTL and aliases are per-space settings, not credentials;
       - the service principal is not a harness credential.
     - **Precedent.** The pattern follows ADR-0058 shared keys and the MCP gateway servers.
  2. **Sealing.**
     - **Stored tokens.** Both tokens are sealed with `keyring.PurposeCredential`, with associated data
       `harborpilot:parent:<conn id>` and `harborpilot:sp:<conn id>`.
     - **Write-only everywhere:** API, CLI, UI, Terraform, audit, change feed and exports. The API shows only `has_*`,
       the version counters, and the binding id found during validation.
     - **Never sent to a node.** Only central's HP client opens these tokens.
     - **Run child tokens.** They are sealed the same way (`harborpilot:run:<run id>`) in `run_gateway_credentials`, so
       an at-least-once dispatch resend reuses the token instead of minting again.
     - **Permissions** go through Casbin:
       - the connection needs `account:write`;
       - space settings need `space_config:write`;
       - status needs space read.
  3. **Transport.**
     - **Central's own client** (validation, mint, revoke, budget status): https is required unless the host is
       loopback or `allow_insecure_http` is set (as in ADR-0035).
     - **The run-facing base URL is checked from the node's side.** The base URL also goes to nodes
       (`ANTHROPIC_BASE_URL`, the Codex `base_url`), and every run request carries its child token. A loopback host on
       central means the node's own loopback on another machine, so the loopback exemption doesn't carry over:
       - the URL delivered to runs must be https, with no loopback exemption, unless `allow_insecure_http` is set;
       - the node checks the URL in the launch payload against the same rule before it writes the secrets file, and
         refuses the launch otherwise. So a central that skipped the check can't make a node send a token over plain
         HTTP;
       - if central reaches HP on another address than runs do (a loopback HP next to central, say), a separate
         run-facing URL is configured: the connection's optional `run_base_url` (HP-T2). Without it, runs use
         `base_url`, which then has to pass the run-facing rule itself (a loopback `http` `base_url` is refused unless
         `run_base_url` or `allow_insecure_http` is set).
     - **`allow_insecure_http` exposes child tokens.** With it, every run's token crosses the network path in clear for
       its whole TTL, as does the parent token on central's calls. It is for tests and a trusted local network only,
       and the connection's status says it is set.
     - Redirects are never followed.
     - Errors keep HP's status, error type and its `error.message`, cut to a fixed bound and treated as untrusted text
       (T-03), so failure reasons stay readable (SM-GW-003). Raw response bodies are never kept.
     - Tokens are never logged.
  4. **The gateway replaces accounts for an opted-in harness.**
     - `LaunchRun` skips account resolution.
     - A launch that names an account is refused (`precondition`).
     - The resolved manifest records `{gateway: "harbor-pilot", project_id}`.
     - If HP is unavailable (no connection, or a mint that fails), the launch or dispatch is refused. **There is never a
       fallback to a direct account**, silent or audited **(Q1)**. A harness configured for HP goes only through HP.
     - A launch is refused early when a blocking HP budget covering the space is `exceeded`. HP stays authoritative and
       enforces with 429 mid-run.
     - **The pre-launch budget check is HP's own: the dispatch mint sends `check_budget: true`** (HP 0.1.3, its
       D-043). HP then refuses a run whose binding, project, workspace or global blocking budget is used up with 429
       `budget_exceeded` and the `budget_id`, and the run fails with that reason before its harness starts. This uses
       HP's own figures and scope rules instead of SpaceMan recomputing which budgets cover the space. The budget
       status that `LaunchRun` reads still gives the earlier refusal and the Limits page (HP-T5). Two limits stay: HP
       doesn't check account budgets at mint, and a budget with spend under its limit passes even when the first
       request is then refused for its own estimate. A mint that fails with 502 (`could not check budgets`) is a mint
       failure, not a budget refusal.
     - When budget status can't be fetched, the space's `on_budget_status_unavailable` decides: `proceed` (the default;
       the gateway still enforces) or `refuse` **(Q3)**.
  5. **A per-run child token, minted at dispatch** (not in `LaunchRun`; see Alternatives):
     - scopes `[gateway:invoke, gateway:models]`;
     - the space's TTL;
     - `bound_refs = {spaceman.space, spaceman.run, spaceman.instance, session, project_id}`, plus `spaceman.task` when
       the run has one.
     - **Bindings shared by many spaces must not pin `spaceman.space`.** HP refuses a bound value that differs from a
       binding's pinned one.
     - **Revocation.** The token is revoked on every end path (exit, failure, stop, relocation, purge, lease loss,
       connection change) through a durable queue written in the end path's own transaction. The TTL is only a backstop.
  6. **Delivery only through the launcher secrets file (ADR-0009).**
     - **Claude Code** gets `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`, and no other Anthropic or Claude credential.
     - **Codex** gets the five `-c model_provider…` overrides with `env_key = "HARBOR_PILOT_TOKEN"`, and that variable.
       No `auth.json` is written, and the node removes any ADR-0054 API-key `auth.json` or ChatGPT-login link from that
       instance's `CODEX_HOME`.
     - **Node protocol.** This needs node protocol minor 9. Gateway launches to older agents are refused with a clear
       reason.
     - **Model choice.** The run's model must be one the HP plan routes. How the default is chosen per space is settled
       in the implementation, through `allowed_model_aliases` or a default model. Since HP 0.1.2, Claude Code's default
       model works: its `system`-role messages are accepted (K-13, resolved by the 0.1.3 re-run). HP forwards them
       unchanged to models not on its no-support list, so a model without support gets the provider's own 400.
  7. **Events and usage.**
     - **Events.** HP events become shipped event templates. Their text is untrusted (T-03).
       - Only project-scoped budget events map to one space.
       - **Unscoped events stay open (Q4).** These are quota, `access.request.blocked`, and binding/workspace/global
         budget events. **Interim default (control's proposal, not the operator's choice): no automatic routing.**
         They reach only the spaces that explicitly subscribe with the template.
       - **Budget refusals arrive as `access.request.blocked`** (HP 0.1.2, its D-040): one event per refused request,
         with `reason` naming the budget, alongside the once-per-period `budget.threshold.exceeded`. The HP-T6
         templates include it. It carries only `request_id` and `binding_id`, so it is an unscoped event under Q4. A
         space-level mapping through the request's `trusted_refs.spaceman.space` would need an HP API read per event
         and is not decided. Its volume (one per refused request) also argues for dedup or a digest.
     - **Usage: per-run usage is in scope (HP-T5).** HP 0.1.2 filters usage by trusted ref (its D-041).
       - **Run-end reconcile.** After a gateway run ends, central reads
         `GET /api/v1/usage/summary?ref=spaceman.run:<run id>&binding_id=<binding>` with the service-principal token,
         and records the run's usage with source **`harbor-pilot`**: tokens, attempts and HP's cost categories, labelled
         as gateway-observed figures. `binding_id` is always passed, because ref values are chosen by the minting
         client and only the binding makes them SpaceMan's.
       - **What it fills.** It gives Codex runs usage, which today is `unavailable`. It never overwrites usage a harness
         reported itself (Claude's transcript usage); how both are shown for a Claude run is settled in HP-T5.
       - **Timing.** HP writes ledger rows as requests finish, so the reconcile runs after the token is revoked (no new
         requests) and may be retried later; a figure read before the run's last request settled is not final.
       - **Reading fails or the connection has no service-principal token:** the run keeps its current usage, never a
         zero.
       - The Limits page also shows project-level HP usage, labelled with its source.
  8. **Minimum Harbor Pilot version: 0.1.3.** SpaceMan relies on 0.1.2's ref filters, budget-refusal events and
     `system`-role support, and on 0.1.3's `check_budget`. hptest and the contract pin v0.1.3, and the docs state the
     minimum. No HP API that reports its own version was found at v0.1.3, so connection validation detects an older
     gateway with a feature probe (HP-T2): the validation mint always sends `check_budget` (false). HP decodes the
     mint body refusing unknown fields, before it checks scopes (`internal/gateway/models.go:230-235` at v0.1.3), so a
     gateway before 0.1.3 answers 400 `invalid body: json: unknown field "check_budget"`, and the connection is
     refused as "older than Harbor Pilot 0.1.3". An older gateway is never used without `check_budget` or per-run
     usage. The probe can't tell a later gateway that dropped the field from an old one; HP's D-043 keeps it.
- **Consequences (security-relevant):**
  - **Agents can read their own child token.** A leaked one is worth one binding's routing and budget for at most the
    TTL. It is labelled with the run and revoked at the run's end. A run that outlives its TTL loses model access,
    because HP can't refresh a token inside a running harness. The 24 h default and the per-space maximum of 7 days
    bound this **(Q2)**.
  - **HP clamps a child's TTL to its parent's expiry** (contract: Mint). A parent that expires sooner than a space's TTL
    silently gives that space's runs shorter tokens, and they lose model access earlier. Validation (HP-T2) learns
    the parent's expiry by minting its probe credential with HP's maximum TTL (7 days) and reading the clamped
    `expires_at`; the connection and the space settings then warn when the parent expires before an opted-in space's
    TTL. The probe's token is discarded at once and the credential revoked; if the revoke fails, the credential goes to
    the durable revocation queue.
  - **Revoking the parent in HP cuts every live run at once.** That gives an emergency stop, and is also a single point of
    failure.
  - **Hard spend limits come only from HP.** SpaceMan's own budgets stay soft (ADR-0036).
  - **An HP outage stops new runs** for the harnesses that opted in.
  - **The parent token is long-lived and high-value.** Its protection rests on central's keyring and KEK, as for accounts.
  - **Evidence ceiling (Q7).** Until a real upstream is available, evidence is "real `serve --demo` 0.1.3 with
    synthetic upstreams" (0.1.1 for the first spike). A live run against a real HP and provider is a placeholder in TRACEABILITY (SM-GW-010); the
    operator says one is coming soon.
- **Alternatives:**
  - **An account kind:** rejected (above).
  - **One parent token per space:** more secrets, no gain.
  - **Minting in `LaunchRun`, before its transaction:** rejected. Relocation may clear the session id inside the launch
    transaction, and a node that stays offline past the TTL would receive an expired token. Early refusal comes from a
    pre-check instead.
- **Open points:**
  - **HP-T4: should the gateway env win over a profile's env in gateway-enabled spaces? Not decided.**
    - ADR-0059 lets a claude-code profile's `claude_settings.env` set any variable, `ANTHROPIC_BASE_URL` included. The
      operator accepted this on 2026-11-07 (ADR-0059 accepted risk, THREAT_MODEL T-32).
    - In a space with HP enabled for Claude Code, a profile `env` could point a run's model traffic away from HP or
      replace its token. Those requests would escape HP's hard budgets and its per-run attribution, so this is a
      budget-bypass consideration, not only a configuration one.
    - The options for HP-T4 are:
      - the node lets the gateway's `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`/`HARBOR_PILOT_*` win;
      - central refuses such profiles for gateway harnesses;
      - or both stay as they are, documented as the operator's choice.
    - Whoever edits profiles can already run hooks on the node (ADR-0059), so the decision is about budget
      enforcement, not about a new privilege.
  - **Who may choose a space's project? Implemented as decided (space_config:write), flagged for review.**
    - HP-T2 follows decision 2: a space's settings need `space_config:write`, which the space's operators hold. An
      operator can therefore point their space at any project the connection's binding allows, another space's
      project included, and so charge that project's budget.
    - Binding an account to a space needs `account:write` (`AccountRules` in `internal/api/accounts.go`) for the
      same reason: it hands a credential's spend to the space. Requiring `account:write` for `enabled` and
      `project_id` (and leaving TTL, harnesses and the budget fallback to space operators) would match that. Not
      changed without a decision.
- **Operator questions and answers (asked and answered 2026-11-06):**
  - **Q1, fallback to a direct account:** never.
  - **Q2, token TTL:** 24 h by default, set per space between 15 min and 7 days.
  - **Q3, budget status unavailable:** a per-space setting, defaulting to proceed.
  - **Q4, unscoped events:** **undecided (open)**; the operator was not sure. The interim default is control's proposal,
    not an operator answer: they are not routed automatically and go only to spaces that explicitly subscribe.
  - **Q5, upstream issues:** yes, through cross-space requests to the Harbor Pilot space in SpaceMan once it exists.
    These cover a ref filter for usage, a service-principal token resource in its provider, project-id outputs in
    scenario 07, the doc mismatches and the K-13 protocol gap. Harbor Pilot 0.1.2 (2026-11-07) answered the ref
    filter, the doc mismatches and K-13; 0.1.3 added `check_budget` on mint.
  - **Q6, per-harness opt-in:** yes, with a `harnesses` list open to any harness HP can serve.
  - **Q7, real upstream:** coming soon. Until then, evidence is real `serve --demo` (0.1.1, then 0.1.3), with a
    placeholder kept for a live run.

### ADR-0061 — More shipped GitHub event templates; optional filters and the `absent` operator
- **Status:** Accepted, 2026-11-07 (operator request: spaces should react to pull request conversation, reviews, CI and
  issue comments).
- **Context:** SpaceMan shipped only `github-issue` and `github-pull-request`. go-pulse-github v0.1.0 publishes every
  webhook it receives on `evt.github.<account>.webhook.<event>.<action>` with GitHub's payload unchanged, so comments,
  reviews and CI completions were available but nothing presented them. The template engine could not express what
  such templates need: a filter that a subscription turns on (one repository or owner, one author to ignore) or off
  (bots, successful runs), and "this field is missing" (GitHub sends comments on issues and on pull requests as one
  `issue_comment` event; only a pull request's has `issue.pull_request`).
- **Decision:**
  - **Engine.** A filter may name a variable in `if_set` (it applies only when that variable has a value) or
    `unless_set` (only when it has none). Bind drops the filters a subscription's values turn off, so the bound
    subscription and its preview show the filters that really apply. A new operator `absent` is the inverse of
    `exists` (missing or JSON null). Both names must be variables of the definition; the web editor checks the same.
    Filters still only narrow what a subscription accepts: the subject is untouched (SM-EVT-010), variables are still
    pattern-checked, and instructions still see only variables.
  - **Six new built-in templates,** seeded as version 1 at central's start when missing (existing installations get
    them on upgrade; existing templates are not touched): `github-pr-comment` and `github-issue-comment`
    (`issue_comment.created`, split on `issue.pull_request`), `github-pr-review` (`pull_request_review.submitted`),
    `github-pr-review-comment` (`pull_request_review_comment.created`), `github-workflow-run`
    (`workflow_run.completed`) and `github-check-suite` (`check_suite.completed`).
  - **Variables.** All six take `account_key` (required) and optional `repository`, `owner` and `exclude_author`
    (the actor's login, for a space's own agent account). Conversation templates ignore bot actors unless
    `include_bots` is `true`; CI templates keep them unless `exclude_bots` is `true` (a bot's pull request can fail CI
    too) and accept only `failure`, `timed_out`, `cancelled` and `startup_failure` unless `include_successes` is
    `true`. `github-check-suite` has an optional `app` (the suite's app slug). Flags accept only `true`.
  - **Untrusted text (T-03).** Item titles carry only values GitHub constrains: repository, number, login, review
    state, CI conclusion and trigger, app slug and a short SHA. Issue and pull request titles, comments, review
    bodies, file paths, workflow, app and branch names go only into the body (as text or inline code) and the raw
    payload attachment. Instructions use only variables, as before.
  - **Correlation and dedup.** Conversation items correlate on `github:<owner>/<repo>#<number>`, the key the
    `github-pull-request` and `github-issue` templates use, so a pull request's events, comments and reviews group.
    CI items correlate on `github:<owner>/<repo>@<head_sha>`, so reruns and both CI templates for one commit group.
    A redelivery is the same event ID and is deduplicated by ingestion as before. Edits and deletions are other
    actions, which these subjects do not match.
  - **Not added.** `check_run` and `workflow_job` (one item per job; the suite and the run summarise them),
    `pull_request_review_thread`, discussions, releases and deployments. Pull request and issue lifecycle actions
    (closed and merged, reopened, ready_for_review, review_requested, assigned, labeled) are already matched by the
    existing templates' `*` action and are not duplicated; their rendering stays as it is, because changing a
    published built-in needs a new version that existing installations would not get.
- **Consequences:**
  - The Terraform provider's typed definition (`TemplateFilter`) lacks `if_set` and `unless_set`, and it refuses
    unknown fields: custom definitions using them cannot be written through it yet, and the new built-ins'
    definitions lose the fields when read through its data source. That is a provider follow-up.
  - The existing `github-issue` and `github-pull-request` templates still put the issue or pull request title into
    the item title. Moving it to the body needs a new published version of each.
  - The fixtures are derived from GitHub's documented payload shapes and pulse-github's normalisation code, not
    captured from a live pulse-github (SM-EVT-002 notes).

### ADR-0062 — CI scope: 250 ms write-transaction guard on hosted runners, path-gated site audit, fuzzing nightly and at release
- **Status:** Accepted, 2026-11-07 (operator decisions Q1 b, Q2 b and Q3 "nightly and release", after the CI duration
  investigation on PR #15).
- **Context:**
  - `storetest` fails a test when a successful write transaction takes longer than `MaxTxDuration`: Hallpass
    HP-DB-004's short-transaction rule, 100 ms by default.
  - On GitHub's hosted runners the non-race coverage job failed it intermittently on unchanged code, at 101–178 ms. It
    failed on `main` before PR #15 as well, so each such run needed a manual rerun. The hosted runners run the same
    tests about 1.7× slower than a developer machine.
  - With the Go checks split into parallel jobs, the site job's axe audit (one Playwright test, about 4.5 minutes) is
    the longest step of most pull requests, including ones that change no page.
  - Fuzz smoke (13 targets × 10 s plus their builds) ran on every pull request and push. Nightly CI already fuzzed each
    target for 10 minutes. The release workflow ran neither fuzzing nor the site audit.
- **Decision:**
  - **Q1 b: the guard is 250 ms on hosted CI.** `SM_TEST_TX_MAX=250ms` is set for the non-race coverage step in
    `ci.yaml` and in the release workflow's test gate. Both run on GitHub-hosted runners, and the release gate flaked
    the same way, so it takes the same value. `make check` and plain `go test` keep the 100 ms default. The race steps
    keep their own relaxed limits (1 s in CI, 500 ms in `make race`). The HP-DB-004 evidence is therefore
    "100 ms locally, 250 ms on hosted CI".
  - **Q2 b: on pull requests the axe audit runs only when the site can change.** The audit runs when the PR changes
    `site/`, `docs/` (included into site pages), `web/` (the audit spec and its tooling), `api/` (the API reference is
    generated from `api/openapi.json`), `CHANGELOG.md` (the changelog page), the `Makefile` or `ci.yaml`. The changed
    files are the diff between the PR's merge commit and its base (first parent), computed in the job with `git`. No
    third-party path-filter action is added. The site build, dead links, screenshot references, Mermaid and the
    API/CLI/configuration reference drift (`make site-verify`) still run on every PR. Pushes to `main`, the nightly run
    and the release workflow always run the full audit (`make site-check`).
  - **Q3: fuzzing runs nightly and at release only.** The PR/push workflow no longer fuzzes. The nightly run fuzzes
    every target for 10 minutes. The release workflow runs the 10 s smoke as a gate.
  - **The release workflow gates on all of them.** It builds no archive or image until vet, race tests, the coverage
    floor (250 ms guard), fuzz smoke and the full site check with the audit have passed.
- **Alternatives:**
  - **Keep 100 ms and rerun failed jobs:** a manual step on most days, and it teaches people to rerun red CI.
  - **Turn the guard off in CI:** it would then be enforced only by whoever runs `make check`.
  - **The audit on every PR, as before**, or **only nightly:** the first keeps the slowest step on PRs that change no
    page; the second finds a site regression a day late.
  - **`dorny/paths-filter`:** one more third-party action with access to the job token, for a single `git diff`.
- **Consequences:**
  - A transaction that takes between 100 and 250 ms on a hosted runner no longer fails CI. That is roughly 60–150 ms
    on a developer machine, and `make check` still fails it there.
  - What the guard exists to catch still fails at 250 ms: network or file I/O, a wait or an unbounded scan inside a
    write transaction costs far more than that. I/O in a transaction is also rejected statically by the `txscope`
    analyzer in every run.
  - A PR that breaks a page's accessibility only through a file outside the list (say, a Go change that alters
    generated reference pages without their regenerated output) is caught by the drift check on that PR and by the
    audit on the next push to `main`, the nightly run or the release.
  - A fuzz regression is found by the nightly run, at the latest by the release gate.
  - When a release fails its coverage step with `write transaction … took … (> 250ms …)`, rerun the job first.
    If it fails again, run that package locally with `make coverage`, which uses the 100 ms default
    (`site/development/testing.md`).
  - Not security-relevant: no product behaviour, default or requirement changes, and the 100 ms rule is still
    enforced by `make check`. Only how much variance CI tolerates on shared hardware changes.

### ADR-0063 — SpaceMan admin MCP: SpaceMan's own API as an MCP server for a space's runs, through a secretless internal token (security-relevant change)
- **Status:** **Proposed**, 2026-11-07. Not yet accepted. The operator's decisions below are recorded with the design.
  Implemented so far (AM-2): migration 0041, the internal token and its principal, the audit attribution and the
  approver exclusion. No server, tool or endpoint uses them yet (AM-3 to AM-10). Requirements SM-MCP-005..009; threat
  T-33.
- **Context:**
  - The operator wants an "infra" space whose agents administer SpaceMan itself: create spaces, launch and stop runs,
    manage tasks, limits, roles, schedules and the other things an administrator does with the CLI or the API. Today an
    agent only has the built-in `spaceman_*` tools (ADR-0012), which are scoped to its own space and run, and the
    gateway's external HTTP servers (ADR-0023).
  - Everything the CLI can do is an API operation (ADR-0040), and every operation has an authorization `Rule`. API
    tokens act for their creator with grants covered by the creator's, are authorized as both token and creator, and
    skip re-authentication and MFA (only a browser session can step up). There are no service principals.
  - This hands administrator power to agents, so it is security-relevant: prompt injection into such a space reaches
    everything the server's grants reach (T-33).
- **Decision:**
  1. **Placement.** A new gateway server kind, `spaceman` (`mcp_servers.kind`, default `http`). It runs **in central,
     in process**. Agents reach it through the existing `spaceman-agent mcp-bridge` → node connection → central tool
     path, granted per space and role with the existing `mcp_grants`. No new binary, transport or node protocol minor.
  2. **Identity: a secretless internal API token** (`api_tokens.purpose = 'spaceman_mcp'`), one per server.
     - Created with the server, in the same transaction, by an administrator. Owner: the creator. Parent: the
       creator's API token when a token created it. Its grants must be covered by the owner's current grants and, when a
       token creates it, by that token's grants. A token issued by another token, the root token and an internal
       principal cannot create one (`auth.CreateInternalTokenTx`).
     - It has **no secret**: its stored MAC is over a random value that is discarded, and `AuthenticateAPIToken`
       refuses any row with a purpose, so it can never be presented as a bearer token. There is nothing to mint, store,
       rotate, deliver or leak: nothing reaches nodes, runs, configuration, the secrets file, the manifest, logs, the
       audit log, exports, backups or Terraform state.
     - Per call, central builds the principal from the token ID (`auth.InternalPrincipal`): it fails like a bad bearer
       token when the token is unknown, not internal, revoked or expired, or its owner is disabled. The principal carries
       the run (`Principal.RunID`) and is authorized as token **and** owner, as every API token is, so it loses what its
       owner loses. Revoking the token (Tokens page, or revoking its parent), disabling the owner, deleting or disabling
       the server (revokes the token in the same transaction, AM-5), revoking the grant or reaching the expiry stops it
       on the next call.
     - "Re-issue" revokes the token and mints a new one in one transaction: for grant changes, owner transfer and expiry
       renewal.
  3. **Audit.** Calls are attributed to the owner, with `via: spaceman_mcp:<token id>` and `run: <run id>`
     (`audit.Actor.Run`, `api.Actor`). The gateway's `mcp_server.tool_called` event also gains the operation and class
     (AM-5).
  4. **Never an approver.** The approver re-check (SM-APPR-003) refuses any `spaceman_mcp:` actor, any actor with a
     run, and any internal token named as an API token. The approval operations themselves are excluded (below).
  5. **Tool surface: one tool per API operation** (operator decision, Q3 a), generated from the build's in-memory API
     document and the `Rule` table, named `<slug>__<operation_id>`. A reviewed classification puts each operation in
     `read`, `write`, `dangerous`, `sensitive-read` or `excluded`, and `TestEveryOperationIsClassified` keeps it
     complete (AM-3). Calls run through the API's own handler chain (authorization, validation, idempotency, audit)
     with the internal principal (AM-4).
  6. **Hard exclusions (kept, operator decision).** Never exposed, whatever the server's settings:
     - every operation whose rule is Public, Session, UserOnly, Reauth or MFA (sign-in, own account, step-up, token
       creation, user writes, root token, break-glass). The API also refuses an internal principal on these rules
       directly, although bearer tokens skip step-up (`authorizeOps`);
     - approvals and approval-policy writes; responding to, assigning, moving or commenting on human actions;
     - takeover and terminal input;
     - operations that return a one-time secret (node enrollment);
     - sign-in provider writes;
     - SpaceMan MCP management, token operations, and gateway operations that target a `spaceman` server;
     - restore acknowledgement.
  7. **Defaults (operator decisions for a trusted infra space).**
     - **Preset: full admin on all spaces** (`admin@*`), labelled "infra" in the UI. Narrower presets (read-only or
       operate, one space or all) stay available.
     - **Grant: the control role** of the chosen space.
     - **Approvals: none by default, but dangerous tools always need one** (Q1 b): deletes and purges, retention runs,
       harness profile settings (T-32), diagnostic launches, SSH key generation and rotation, account and event
       connection secrets, prompt, skill, role template and human-type publication, HTTP MCP server and grant writes,
       node state and updates, relocations. This needs a synthetic one-use policy in `internal/approvals`
       (`Gate.Require`, AM-5). Writes can be gated per server.
     - **Expiry: never** by default (Q2 a); 1–366 days on request.
     - Only control and worker runs get the tools; KB, dreamer and diagnostic runs never do.
  8. **External endpoint in this release** (Q4 b, AM-10, SM-MCP-009). Central also serves the tool surface at
     `/api/v1/mcp` for operators' own MCP clients, authenticated with a bearer API token (never an internal one), with
     the same classification, exclusions and approval rules.
  9. **Terraform:** a new resource `spaceman_admin_mcp_server` (grants, preset, tools, approval, expiry; computed token
     ID, owner, expiry and effective tools; no secret attributes), granted with the existing `spaceman_mcp_grant`
     (AM-9).
  10. **Numbers:** migration 0041 (four `ADD COLUMN`s: `mcp_servers.kind`, `api_token_id`, `spaceman_json`;
      `api_tokens.purpose`, forward-only like every migration), T-33, SM-MCP-005..009. No protocol minor.
- **Alternatives:**
  - **A first-class service principal** (`svc:<id>`), independent of any person. Access would survive admin turnover
    without a re-issue, but it roughly doubles the authentication work (new subject kind, grants, management, audit) and
    adds an identity nobody is accountable for. Rejected for now (Q1).
  - **Register central's own HTTP API as an ordinary `http` server**, with a bearer token in its sealed headers. It
    needs a private-address exemption for loopback, keeps a real admin secret at rest, and adds a network hop to itself.
  - **A node-side binary with the token in the run's secrets file.** It puts an administrator secret within the agent's
    reach on the node, needs a protocol minor and collides with Harbor Pilot's secrets delivery (ADR-0060).
  - **Three meta-tools** (search, describe, call) instead of one tool per operation: smaller catalogs, but every call
    is opaque to approval policies and to the harness's own tool permissions. Revisit if `admin@*` (about 150 tools)
    strains a harness's context (Q3).
  - **Read-only, one space, 90-day expiry and writes gated by default** (the plan's recommendation): safer defaults,
    but the operator chose full admin, no expiry and no approvals for their trusted infra space; dangerous tools stay
    gated either way.
- **Consequences:**
  - With the defaults, an agent in the infra space can do what its owner can, except the hard exclusions and without a
    person's approval for anything but dangerous tools. Prompt injection into that space is accepted risk T-33.
  - Access depends on a person: if the owner is disabled or loses grants, the server stops until someone re-issues it.
  - Tools granted later reach runs launched afterwards; revocation applies on the next call, while a revoked server's
    tools stay listed until the harness restarts.
  - A future operation added without a classification fails `TestEveryOperationIsClassified`; a misclassified one is a
    review risk.
  - Secrets that an agent writes through these tools (account keys, event connection secrets) pass through the model's
    context; those operations are dangerous and need an approval.

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
| K-9 | ~~Codex ChatGPT-login token materialization (`auth.json`) vs API key.~~ **Resolved 2026-11-03:** the `codex_chatgpt_login` kind, one live copy per account linked into each `CODEX_HOME` (ADR-0053); a real ChatGPT login is not verified (no account). | SM-ACCT-002 | done |
| K-11 | ~~Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26).~~ **Resolved 2026-11-02:** SSO grants are stored under `user:<id>#sso`, which only SSO sessions act as (ADR-0029); SR-26 ported as `TestReviewSR26SSOGrantsOnlyForSSOSessions` (`internal/auth`, `internal/oidc`) and `TestOIDCSignInOverHTTP`. | — | done |
| K-10 | ~~Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode.~~ **Resolved 2026-11-02:** works interactively with an empty config dir once first-run state and folder trust are seeded (spike S-5, ADR-0021). | — | done |
| K-12 | Harbor Pilot integration design questions Q1–Q7. Answered by the operator on 2026-11-06 (ADR-0060), except **Q4**: routing of unscoped HP events (quota, access-blocked, binding/workspace/global budgets) is undecided; the interim default (control's proposal) is no automatic routing, delivery only to spaces that explicitly subscribe. Q7: a real upstream is "coming soon". | ADR-0060 acceptance; SM-GW-007, SM-GW-010 | ADR-0060 stays Proposed; Q4 open; live-run placeholder in TRACEABILITY |
| K-13 | ~~Harbor Pilot 0.1.1 refuses Claude Code 2.1.317's `system`-role entries in `messages[]` (seen with the default Claude 5 model; `claude-sonnet-4-5` works).~~ **Resolved 2026-11-07:** Harbor Pilot 0.1.2 accepts them (0.1.3 downgrades them only for models known not to support them, its D-042). Re-run with Claude Code 2.1.317 on its default `claude-opus-5-5` through `serve --demo` v0.1.3 with a minted child token: `["user","system"]` got 200 and the turn completed, passed through unchanged on the demo's Anthropic-shaped upstream (`docs/contracts/harnesses.md`, Harbor Pilot 0.1.3 re-run). A real Anthropic upstream is not verified (demo upstreams only). | — | done; SpaceMan requires Harbor Pilot ≥ 0.1.3 (ADR-0060) |
