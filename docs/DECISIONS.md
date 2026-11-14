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
  - *Update 2026-11-12 (ADR-0076), tightening:* the page compiles the package's `ghostty-vt.wasm`, served as a hashed
    asset from its own origin, and no longer uses the inlined `data:` module. `connect-src` is back to `'self'` alone.
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
    hang. *Amended 2026-11-09:* the reset only hid part of the cause and is gone. ghostty-web's shared instance does not
    clear a freed terminal's memory, so the next terminal started out with its cells (another run's lines showed
    through, reset or not), and memory of a terminal with many styles was never reused (the page grew until the tab
    crashed). Each terminal now runs in a WebAssembly instance of its own, released with it
    (`web/src/features/terminal/ghostty.ts`, `wasm.test.ts`, `e2e/terminal-switch.spec.ts`; since ADR-0076 the instances
    share one compiled module, never memory). The view follows the cursor's row as output arrives (until the viewer scrolls away), and the wheel scrolls
    the box before it reaches ghostty-web's scrollback.
  - **Connections.** One change-feed stream serves all tabs of a browser: the tab holding the Web Lock
    `spaceman-watch` opens it and relays it over a BroadcastChannel; another tab takes over when it closes. Without
    Web Locks (not a secure context: plain HTTP to another host) each tab keeps its own stream. A tab hidden for 5 s
    releases its live terminal and resumes from the last record it drew when shown again.
- **Consequences:**
  - Control and workers get notices after every turn, as SM-COMM-003 intended. The fake runner always behaved this
    way, which is why the tests did not catch it; `TestRealSendIdleOrDone` now runs against real herdr-core.
  - `make e2e` reopens a full-screen run terminal (the stub harness's `/fullscreen` prints the start-up with neutral
    text); without the reset the journey hung (with an instance per terminal, since 2026-11-09, it needs none).
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
- **Note (2026-11-07, human responses):** a response whose requester's run has ended went to control only while a
  control run was wanted (ADR-0032), so with no control running it became undeliverable: auto-start never saw it, and
  a control started later was not told about it. With auto-start on, the routing now also sends it to control when the
  space can start one (it had a control run or has a space role of kind control), as a pending, waking notification.
  The sweep then starts control for it, and that control, or one an operator launches later, gets one notice per
  unacknowledged response. With auto-start off, or with no control to start, it stays undeliverable as before.

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
  - **Note (2026-11-07, ADR-0064):** SpaceMan adds one key of its own, `"tui": "default"` (Claude Code's classic
    renderer), when the profile's settings have no `tui`. The full-screen renderer, Claude Code's default for a fresh
    configuration, keeps earlier output out of the terminal, so watchers couldn't scroll back without a takeover. A
    profile's `tui` always wins, and the profile's other keys are written unchanged.

### ADR-0060 — Harbor Pilot gateway: one central connection, spaces opt in per harness, per-run child tokens (security-relevant change)
- **Status:** **Accepted**, 2026-11-08 (HP-T8), with the operator's answers to Q1–Q7 of 2026-11-06 and their later
  decisions (listed at the end; the points they settle are marked **(Qn)**). Proposed 2026-11-06. Accepted with:
  - **Project choice: option b** (2026-11-07). Setting or changing a space's `project_id` needs `account:write`, like
    binding an account to a space (decision 2, "Open points").
  - **Minimum Harbor Pilot version 0.1.3** (decision 8), checked by the mint probe. **0.1.4 recommended** (2026-11-08,
    below).
  - **Q3 as built:** a per-space `on_budget_status_unavailable`, `proceed` by default or `refuse`. Since HP-T8 it also
    covers SpaceMan's own failures while reading status (a token that can't be opened, a database error), with a fixed
    reason.
  - **Q4 stays open.** Where unscoped Harbor Pilot events go (quota, `access.request.blocked`, binding, workspace,
    account and global budgets) is **not decided** and is not decided by this acceptance. The interim default is
    control's proposal, not an operator answer: no automatic routing, delivery only to spaces that subscribe
    (DECISIONS K-12).
  - **Q7 is not met yet:** all real evidence is `serve --demo` 0.1.3 with synthetic upstreams; a real model provider
    behind a real gateway is **not verified** (evidence map: RELEASE_READINESS §13).
  - **Implemented:** HP-T2, the connection and space settings (decision 1; decision 2 for the stored tokens; decision 3's
  central-side rules) and the version probe of decision 8. HP-T3: central's side of decisions 4 and 5 (routing without
  fallback, mint-or-reuse at dispatch, revocation on every end path; see "Implementation notes (HP-T3)" below). HP-T4:
  decision 6, delivery to the harness on the node (protocol minor 9; see "Implementation notes (HP-T4)" below). HP-T5:
  the budget pre-check, the Limits card and per-run usage (decisions 4 and 7; see "Implementation notes (HP-T5)"
  below). HP-T6: event templates (§7). HP-T7: Terraform resources and the combined example, in terraform-provider-spaceman.
  HP-T8: verification, the threat model (T-03, T-05, T-34 to T-37, T-39), LIMITATIONS and release notes. Contract:
  `docs/contracts/harbor-pilot.md` (v0.1.3); live spike: `docs/contracts/harnesses.md` (Harbor Pilot gateway spike, and
  the 0.1.3 re-run).
  - **Updated 2026-11-07 for Harbor Pilot 0.1.2/0.1.3**, which answered SpaceMan's upstream requests (Q5): per-run usage
    by trusted ref, budget refusals as `access.request.blocked`, `system`-role messages (K-13 resolved), the minting
    credential as `created_by`, and an optional budget check on mint. **The minimum Harbor Pilot version is 0.1.3**
    (decision 8).
  - **Updated 2026-11-08 for Harbor Pilot 0.1.4**, which answered SpaceMan's two later requests (PLAN §4 D-20's
    candidates, filed through a cross-space request): a version endpoint (`GET /api/v1/system`) and budget status
    filtered by project (`?project_id=…&binding_id=…`). Both are **closed**. SpaceMan reads the version with the
    service-principal token and, when it answers, reads only the covering budgets; a 404 means older than 0.1.4 and
    keeps the 0.1.3 path (every budget, workspace looked up). The minimum stays 0.1.3 (decision 8 is unchanged, its
    probe still checks it); 0.1.4 is recommended. 0.1.4's one-time budgets (`period_end: null`, states `scheduled` and
    `expired`) and `remaining_e10` are handled; only `exceeded` with `block` refuses, so no new state can refuse a
    launch (decision 4 unchanged). Not a security-relevant change: no requirement, guarantee or default is relaxed.
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
       - the connection needs `account:write`, and its writes (replace, delete) a step-up (recent sign-in and second
         factor), as event connections do;
       - space settings need `space_config:write`, except the **project**: setting or changing a space's `project_id`
         needs `account:write` (a global administrator or the root token), because the project decides who pays, as
         binding an account to a space does (`set-space-accounts`). The operator decided this on 2026-11-07 (option b
         of the HP-T2 review). An empty `project_id` on an update keeps the stored project, so a space's operators can
         still enable, disable or tune their space; the audit entry records `project_changed`;
       - status needs space read.
     - **Stored tokens go only to the address they were entered for.** A connection write that changes `base_url` or
       `run_base_url` (scheme, host, port or path), or newly allows insecure HTTP, must carry `parent_token` again, and
       `service_token` (or `remove_service_token`) when one is stored; otherwise it is refused with 422 before any
       outbound call. Without this rule, whoever could write the connection (or a stolen administrator session) could
       point it at their own host and receive the write-only tokens in the validation call (HP-T2 review, finding 1).
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
     - Redirects are never followed, and no proxy is used (one from the environment would see every token).
     - **SSRF stance.** `base_url` is chosen by an administrator (`account:write`). Central's client refuses to dial
       link-local (cloud metadata), unspecified and multicast addresses, checked on the resolved address. Private and
       loopback addresses stay allowed on purpose: a Harbor Pilot gateway usually runs on an internal network. What
       comes back from a wrong target is bounded (status, error type and a 300-character sanitised message), and the
       tokens are only ever sent to the address they were entered for (decision 2). THREAT_MODEL T-34 (T-33 is taken by an open branch).
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
       - **Shipped (HP-T6, 2026-11-07):** `harbor-pilot-budget` (`budget.threshold.*`; optional `project_id`,
         `budget_id`, `exceeded_only`, `blocking_only`), `harbor-pilot-quota` (`quota.window.*`; optional `account_id`,
         `monitor_id`, `exhausted_only`) and `harbor-pilot-access-blocked` (`access.request.blocked`; optional
         `binding_id`, `budget_only`, which matches the 0.1.3 reason prefix `budget `). Filters use ADR-0061's
         `if_set`. Correlation: a budget and its period (warning and exhaustion group), a quota monitor (exhaustion
         and recovery group), a binding (a burst of refusals groups); redeliveries carry the same event id and are
         duplicates. Titles and correlation keys use only values Harbor Pilot constrains, and each template's input
         schema enforces those formats, so an event with other text there is quarantined rather than rendered. No
         digest yet; the interim Q4 default stands.
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
     **Since 0.1.4** HP reports its version (`GET /api/v1/system`, `config.read`); SpaceMan reads it for display and to
     choose the budget-status path, never to replace the probe (it needs a service-principal token, the probe doesn't).
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
  - ~~**HP-T4: should the gateway env win over a profile's env in gateway-enabled spaces?**~~ **Settled in HP-T4
    (2026-11-08) by control's instruction that no other Anthropic or Claude credential may sit next to the gateway's:
    the second option, refused, done on the node** (see "Implementation notes (HP-T4)"). Kept as built at acceptance
    (2026-11-08).
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
  - ~~**Who may choose a space's project?**~~ **Resolved: the operator's decision of 2026-11-07 (option b).**
    - HP-T2 first let `space_config:write` set the project, so a space's operators could point their space at any
      project the connection's binding allows, another space's project included, and charge that project's budget.
    - Now setting or changing `project_id` needs `account:write`, like binding an account to a space (decision 2). The
      check runs before the project probe (an unauthorised caller causes no call to Harbor Pilot) and again in the
      write transaction against the stored project.
    - **Residual risk (accepted, not changed):** a space's operators can still disable Harbor Pilot for their space,
      or delete its settings. Its runs then go back to the space's direct accounts and SpaceMan's soft budgets
      (ADR-0036), outside Harbor Pilot's hard limits. Q1 forbids a per-run fallback while the space is enabled, not
      this. If it matters, `enabled` going from true to false could need `account:write` too.
- **Implementation notes (HP-T3, 2026-11-07).** Choices made while implementing decisions 4 and 5; none weakens them.
  - **Hook shape.** `fleet.Options.Gateway` (`fleet.Gateway`, implemented by `harborpilot.Lifecycle`) has three parts:
    `PreLaunch` before the launch transaction (no connection; the budget part is a `BudgetChecker` that HP-T5 fills),
    `Route` inside it (instead of account resolution for a routed harness), and `Token` at dispatch. The gateway and
    project are stored in the run's launch spec (`launch.gateway`) and manifest, so a dispatch uses what the launch
    decided, and a space that later disables Harbor Pilot doesn't move its pending runs to an account.
  - **Allowed aliases.** With a non-empty list, a launch must name one of the models; a launch with no model (the
    harness default) is refused, since central can't tell what it would use.
  - **Session.** A fresh Claude Code gateway run gets its session ID from central (a UUID chosen after the relocation
    check), sent with `resume: false`; the run row learns it from the node's report as before, so a run that never
    started has none and isn't resumed. Codex and resumed runs bind their own session; a fresh Codex run binds the run ID.
  - **Token storage.** The sealed child token is dropped as soon as its revocation is queued (not when the revocation
    is done): nothing needs it after its run's end. A row without a token means the run's token is gone, and a later
    dispatch of that run fails as `gateway_unavailable` instead of minting again. A token minted for a run that was
    stopped meanwhile is queued for revocation at once.
  - **Revoker.** It runs on central's deadline timer (ADR-0036), woken by the commit that queued a revocation, at most
    10 revocations per round with 5 s per call. 204 settles as revoked. 404 and 401 settle as revoked only when the
    parent that asked is the one that minted the credential (a 401 then means the parent itself is revoked or expired,
    which ended its children; audited). From any other parent they prove nothing and settle as failed. 403 and other
    refusals settle as failed. Failed rows are audited (`harbor_pilot.revocation.failed`), logged, counted in the
    connection's `failed_revocations` and on its page, and in the `spaceman_gateway_revocations_failed` gauge. Network
    failures, 5xx and 429 are retried after 1 s, ×4 each time, at most 5 min, until the credential expires. No human
    action is raised yet.
  - **Which parent minted a credential (review F1, 2026-11-07).** `run_gateway_credentials` and `gateway_revocations`
    record `parent_fp`, the first 8 bytes of a domain-separated SHA-256 of the minting parent token, hex-encoded. The
    tokens are random, so this reveals nothing usable. NULL means unknown, and only a 204 counts for such a row. The
    columns are in migration 0040, which is not released yet, so no migration was added.
  - **Parent rotation.** Harbor Pilot answers 404 to a parent revoking another parent's child, so queueing these for
    later would record them as done while they stay valid. Replacing the parent therefore revokes with the old parent,
    at the stored address, everything it minted that may still be valid: live run tokens and pending revocations (for
    example ones queued while Harbor Pilot was unreachable). This happens before the new parent is saved. The outcomes
    are recorded, and the warning says those runs have no model access until restarted. One that can't be revoked is
    recorded as failed with the advice to revoke the old parent in Harbor Pilot (its cascade ends the children). The
    window where a run token is minted during the rotation is closed from both sides:
    - the connection write's transaction lists the old parent's credentials again and, if one is new, runs another
      revocation round (at most 3, then the write fails with "retry");
    - a mint whose store transaction finds the parent version changed revokes its own credential with the old parent
      (queueing it if that fails) and mints again with the new one.
    If the connection write fails after the revocations (a version conflict, another binding while spaces use Harbor
    Pilot), the old parent stays and those runs keep tokens that no longer work. That fails safe:
    `TestParentRotationWriteFailureFailsSafe`.
  - **Lease loss.** Runs a node left `unknown` keep their tokens while it may come back; once the space's lease has
    expired by the disconnect grace plus one minute (the node's interrupt grace), the node's lease guard has stopped
    them and the sweep queues their revocation. The runs stay `unknown`.
  - **Agents before HP-T4 are refused (review F2, 2026-11-07).** A node whose agent doesn't know
    `SPACEMAN_GATEWAY_TOKEN` drops it, and the harness then starts on whatever login the node user has. That is a
    fallback to a direct account, which Q1 forbids. A gateway launch to a node below `proto.MinorGatewayToken` (9)
    therefore fails as `agent_too_old`, before anything is minted: "gateway runs need protocol minor 9, which delivers
    the run's token to the harness; the node negotiated 8: upgrade its agent". HP-T4 raised `MaxMinor` to 9 and removed
    the test-only `GatewayMinMinor` seam that HP-T3 needed before any agent spoke minor 9.
  - **A mint holds up dispatch for at most 5 s (review F3).** A node's dispatch loop waits at most 5 s
    (`fleet.Options.GatewayMintWait`) for a new launch's token. A slower mint (Harbor Pilot retries take about 75 s at worst) goes
    on in the background, once per run and bounded to 2 min. The launch stays pending, the node's other commands
    (stops and kills included) are sent, and the finished mint kicks the loop, which then reuses the stored token.
    Only the pass that starts a mint waits for it; while it runs, later passes skip that launch at once, so many slow
    mints don't add up before a stop (HP-T3 review N1, fixed in HP-T4). Central's shutdown waits up to 5 s for
    background mints, then cancels the rest and waits for them before it closes the store. A cancelled mint that already
    reached Harbor Pilot still queues its credential for revocation (N3).
  - **Failed revocations decay (HP-T3 review N2, fixed in HP-T4).** `failed_revocations` and the gauge count only
    failed rows whose token hasn't expired yet. The deadline timer also wakes at the next such expiry, so the gauge
    drops back without other activity. The rows are kept for the record.
  - **Known gaps.** A crash between a successful mint and the transaction that stores the token leaves a child that
    SpaceMan doesn't track; it stays valid until its TTL. The deadline timer that runs the revoker and the lease-loss
    sweep runs only in `spaceman serve`; anything queued by another process is done at the next `serve` start. Both
    are in LIMITATIONS.
- **Implementation notes (HP-T4, 2026-11-08).** Decision 6 on the node; none weakens it.
  - **Wire shape (minor 9).** At dispatch, central adds `gateway: {kind: "harbor-pilot", base_url, allow_insecure_http}`
    to the launch next to `secrets[SPACEMAN_GATEWAY_TOKEN]`. `base_url` is the connection's `run_base_url`, or its
    `base_url`. The plan's `kind: anthropic | openai-responses` was not used: the harness, not the gateway, decides the
    API, so the kind names the gateway. The address is read at dispatch (`fleet.Gateway.Address`), after the token.
    Moving the gateway needs the parent token again, which revokes every run token (decision 2), so a launch never
    carries a token to an address it wasn't minted for. Both are dispatch-time only: the stored command has neither.
  - **The node checks before it writes the secrets file.** `proto.GatewayLaunch.Origin`: absolute https, no
    credentials, query or fragment; plain http only with `allow_insecure_http`, and no loopback exemption. A launch
    whose token and address don't travel together, or that carries any other secret or names an account, is refused
    (`build launch`). That would be a central bug, and the node doesn't guess.
  - **Claude Code.** `SecretEnv` is exactly `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`. The shim scrubs every
    inherited `ANTHROPIC_*`, and now also `HARBOR_PILOT_TOKEN` and `SPACEMAN_GATEWAY_TOKEN`. The ADR-0021 seed is kept.
    A stored login (`$CLAUDE_CONFIG_DIR/.credentials.json`, from a `/login` inside an earlier run) is removed before
    the launch. A profile whose `claude_settings` would add another credential or endpoint fails the launch with the
    key named. That covers `apiKeyHelper`, `proxyAuthHelper`, `awsCredentialExport`, `awsAuthRefresh`, `gcpAuthRefresh`
    and `forceLoginMethod` (from the 2.1.317 settings schema), and `env` entries `ANTHROPIC_*` and every
    `CLAUDE_CODE_*` but an allowlist of output and UI switches (2.1.317 has over 800, among them OAuth tokens, key file
    descriptors, host-managed auth, base URLs, proxies, client certificates and Bedrock/Vertex/Foundry switches; the
    HP-T4 review asked for the allowlist shape so new ones are refused), plus `HTTP(S)_PROXY`/`ALL_PROXY` in either
    case, `NODE_TLS_REJECT_UNAUTHORIZED`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `SSL_CERT_DIR` and `NODE_OPTIONS`.
    The check is on the node, so it holds whatever central sent.
  - **Codex.** The five `-c model_provider…` overrides from the HP-T1 spike, `base_url = <origin>/v1`, and
    `HARBOR_PILOT_TOKEN` in `SecretEnv`. No `auth.json` is written. Any `auth.json` in the instance's `CODEX_HOME` is
    removed, and the launch fails if it is still there. That covers an ADR-0054 API-key file, and a link to an ADR-0053
    login, whose `auth.lock` link goes too.
  - **Codex's shell snapshot is off for every Codex run** (`-c features.shell_snapshot=false`). The real HP-T4 run found
    that codex-cli 0.166.0 writes the run's environment, `HARBOR_PILOT_TOKEN` and `SPACEMAN_RUN_TOKEN` included, to
    `$CODEX_HOME/shell_snapshots/*.sh`, where it outlives the run (`docs/contracts/harnesses.md`, THREAT_MODEL T-36).
    Every Codex launch, gateway or not, also removes the `shell_snapshots` folder an earlier run left
    (`Launch.RemoveDirs`: a link is removed itself, `os.RemoveAll` never follows one inside, and the launch fails if it
    is still there): on earlier releases it could hold a direct account's API key, which a later gateway run of the
    same instance must not start next to (HP-T4 review F1). Operators rotate such keys (CHANGELOG).
  - **Codex `config.toml` and the provider (HP-T4 review).** With codex-cli 0.166.0 the `-c model_provider…` overrides
    win over a `config.toml` root `model_provider`, a `<name>.config.toml` profile (even under `--profile`) and a
    `[model_providers.harbor-pilot]` table there; a legacy `profile =` key makes Codex refuse to start. So no
    `-c profile=` is passed and nothing is refused (LIMITATIONS).
  - **Model choice.** Not changed. Codex's own default model may not be routed by the plan (the demo refuses it with
    403), so a gateway Codex run needs the role's model or the space's allowed aliases. This is in LIMITATIONS.
  - **Evidence.** Unit tests (`internal/harness`, `internal/proto`, `internal/agent`, `internal/fleet`), the fleet e2e
    test with the real node agent (`TestHarborPilotGatewayDeliveryOnNode`), and real Claude Code 2.1.317 and codex-cli
    0.166.0 runs through SpaceMan against `serve --demo` v0.1.3 (`TestRealHarborPilotGatewayRuns`, 2026-11-08).
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

- **Implementation notes (HP-T5, 2026-11-08).** Decisions 4 and 7; none weakens them.
  - **Which budgets cover a space.** Budget status has no filters, so central selects: `project` = the space's
    project, `workspace` = that project's workspace (looked up with `config.read` only when a workspace budget exists,
    cached 10 min), `binding` = the connection's client binding (every run token is minted under it, so its trusted
    refs carry it), and `global`. `account` budgets are left out: they cover whichever upstream account serves a
    request, not a space, and Harbor Pilot doesn't check them at mint either. A disabled budget is shown as disabled.
  - **Cache.** One budget status read per connection, reused for 30 s (a failed read for 5 s), single flight, read
    outside any transaction. A cached read is keyed on the connection's id, version, service-token version and
    address, so a connection write is never answered from an older read.
  - **Refusal.** A covering budget that is enabled, enforced with `block` and `exceeded` refuses the launch in
    `PreLaunch` with `rate_limited`, for example: `Harbor Pilot budget "daily" (project prj_web) is used up for
    this day: 0.12 of 0.10 USD spent; it resets at 2026-11-09T00:00:00Z`. Names and ids are Harbor Pilot's text, so they
    are sanitised and bounded (T-37). Warn-only budgets never refuse. Control's auto-start already turns a launch
    error into "Control could not start" with the error, so nothing was added there. Status that can't be read (no
    service-principal token, a failed read, a failed workspace lookup) follows `on_budget_status_unavailable`:
    `proceed` logs and goes on, `refuse` answers `unavailable` with the reason.
  - **Mint check.** The dispatch mint sends `check_budget: true`. Harbor Pilot's 429 `budget_exceeded` is not retried
    and fails the run as `gateway_unavailable` with Harbor Pilot's message. Its 502 "could not check budgets" is
    retried like any 5xx, then fails the run the same way (decision 4: a mint failure, not a budget refusal).
  - **Status API.** `GET /spaces/{id}/harbor-pilot/status` (space configuration read): the overall state (`off`, `ok`,
    `warning`, `exceeded`, `unavailable`), the covering budgets with decimal amounts, and the project's usage from
    `usage/summary?group_by=project` since the start of the project budget's current period (the last 30 days
    without one). The usage is labelled `clients: all`.
  - **Per-run usage: how both are shown for a Claude run (the point decision 7 left to HP-T5).** The read-back is kept
    in its own table (`run_gateway_usage`, migration 0043) and returned as `gateway` on the run's usage, next to
    what the harness reported. It is written into `run_usage` (source `harbor-pilot`) only when that row is missing
    or `unavailable`, or is its own earlier fill. A harness's report replaces a fill, even a final one, and a node's
    `unavailable` report never replaces a fill. A node can't claim the `harbor-pilot` source.
  - **When.** The end path's transaction queues the read-back next to the revocation (every path that queues a
    revocation, and also when the token was already given up), first due 15 s later. Reads are at 15 s, 2 min and
    10 min after the end; each replaces the last, and the third is final. A network failure, 5xx or 429 is retried
    (30 s, doubling, at most 5 min) until an hour after the end, then the row fails. A refusal fails it at once. No
    service-principal token or connection settles it as `unavailable`. The reads run on central's deadline timer, at
    most 10 per round with 10 s per call, like the revoker. Query: `group_by=model`, `ref=spaceman.run:<run>`, the
    connection's `binding_id`, and `from`/`to` around the run's lifetime. A model whose every request had no
    reported usage gets unknown (nil) token counts. The cost estimate is Harbor Pilot's `actual_api_charge` in USD if
    present, else its `estimated_api_charge`, labelled with its source.
  - **Numbers.** Migration 0043. 0042 is retired and never used: the admin MCP branch (#18) took 0044, and goose runs without `AllowMissing`, so a 0042 landing after 0043 had been applied would fail to migrate. THREAT_MODEL T-37. PLAN D-18.

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
  - **Follow-up (2026-11-08, ADR-0072):** `github-pull-request` version 2 adds `exclude_author`, `ignore_updates`
    and the optional scope filters and moves the title to the body; built-ins can now gain versions on existing
    installations. The provider's `TemplateFilter` gained `if_set` and `unless_set` in terraform-provider-spaceman
    v0.1.0, and its acceptance test round-trips them (checked against version 2 as well). `github-issue` is unchanged.
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
- **Update, 2026-11-08 (gate wait time, T9):** the Q2 b path rule moved from `ci.yaml` into
  `scripts/site-affected.sh`, its one home; the list is unchanged except that the script itself is added. CI calls it
  with `HEAD^1` on a pull request. A local `make site-check` calls it against the merge base with `main` (committed,
  uncommitted and untracked files) and skips the audit when nothing site-affecting changed; it runs the audit when it
  can't tell, and `SM_SITE_A11Y=1` forces it (the release workflow sets it, so the release still always audits). The
  audit itself is sharded over `SM_A11Y_SHARDS` parallel tests and workers (two in CI) and waits for VitePress to
  hydrate instead of for network idle; it audits the same pages, schemes and rules.
- **Update, 2026-11-09 (every hosted Go test job):** Q1 b covers every non-race job on a hosted runner that opens a
  store, not only the coverage steps. The login-timing jobs (ADR-0071, `ci.yaml` and the release gate) and the fuzz
  jobs (nightly and the release smoke; `internal/keyring`'s targets open a store) ran with the 100 ms default. The
  login-timing job failed on it with `auth.login_failed` holding the lock 120–186 ms while its timing check passed.
  They now set `SM_TEST_TX_MAX=250ms` too. The race steps keep 1 s. Not security-relevant, for the reasons above.
- **Update, 2026-11-13, coverage at 1 s (operator decision):** during the v0.1.2 merges the hosted coverage job
  failed repeatedly on transactions holding the lock 260–750 ms in packages the pull requests didn't touch (auth,
  human, oidc, keyring, messages), and passed on rerun. Shared runners vary more than the 250 ms margin allows. The
  coverage step in `ci.yaml` and in the release gate now sets `SM_TEST_TX_MAX=1s`, the same as the race steps. Local
  runs keep the 100 ms default and the full gate (`SM_TEST_TX_MAX=500ms make check`) keeps 500 ms, so a slow
  transaction is still caught before a pull request; I/O in a transaction is still rejected by `txscope`. The
  login-timing and fuzz jobs keep 250 ms. Not security-relevant, for the reasons above.
- **Update, 2026-11-13, hold analysis (why transactions held the lock so long; limits unchanged):** The v0.1.2 merges still tripped
  the guard: `keyring.load` held the lock up to 3.7 s and `auth.touch_session` 1.33 s in the race job. Per-transaction
  tracing measured three things: the hold (ADR-0070: from BEGIN IMMEDIATE returning to the end of the commit, so lock
  waits never counted), the callback and commit split, and the transaction's own on-CPU time (thread CPU time). The
  work inside the transactions was small: under `-race`, p50 8 ms and p99 32 ms of CPU. `keyring.load` used 12 ms
  (its AES-GCM work is negligible); `touch_session`, a single primary-key UPDATE already debounced to once a minute,
  used about 1 ms. About 60% of the in-transaction CPU was SQLite compiling statements. The holds came from three
  causes.
  - **The first commit flushed the seeded database.** `storetest.Seed` copied a 1.2 MB migrated database without
    syncing it. The store's first commit (`keyring.load`, in every test) creates the WAL, and SQLite fsyncs the new
    WAL and its directory. On ext4 that flushed the whole fresh copy while the lock was held. This node's `/tmp` is
    tmpfs, which hid it; hosted runners use ext4. With `TMPDIR` on ext4, 4 CPUs, `-race`, `keyring.load` commit took
    up to 320 ms, and 23 ms once `Seed` fsyncs its copy and directory.
  - **CPU starvation.** On 1 CPU, holds were 10–25 times the transaction's CPU: the goroutine was runnable but not
    running. More than half of the test processes' CPU went to `store.Open`, where every reader connection
    (`max(4, GOMAXPROCS)`) parses the whole schema, and about a quarter to `checkptr` instrumentation of modernc's
    transpiled SQLite. `checkptr` also accounted for half of the CPU inside transactions. Test stores now open
    `storetest.Readers` (2) reader connections; internal/store still tests the production size. `scripts/race.sh`
    passes `-gcflags=modernc.org/...=-d=checkptr=0`: the race detector stays on everywhere, and `checkptr` stays on
    for our own packages. Non-race runs have no `checkptr`.
  - **Argon2id inside two transactions.** `auth.breakglass` (new administrator) and the `auth.login` rehash of an old
    parameter version derived the key while holding the lock. They now hash first.
  - **Result:** `-race` over auth, oidc, human, messages, keyring and audit (`-p 4 -parallel 4`, pinned with
    `taskset`), hold p50 / p99 / max in ms:

    | disk, CPUs | before | after |
    |---|---|---|
    | ext4, 4 | 28 / 144 / 418 | 18 / 86 / 145 |
    | ext4, 1 | 98 / 581 / 817 | 70 / 331 / 526 |
    | tmpfs, 4 | 21 / 140 / 218 | 11 / 71 / 106 |
    | tmpfs, 1 | 82 / 519 / 799 | 35 / 300 / 391 |

    The remaining holds on one CPU are scheduling waits, not work. The heaviest transactions (`human.*`) use about
    15 ms of CPU.
  - `synchronous` stays `FULL`, and no limit changes. Committed transactions must survive a power loss (HP-DB-004
    is about holding the lock briefly, not about durability), and a test that waits for an fsync measures what
    production does.
  - Not done, recorded as follow-ups: a prepared-statement cache on the writer (statement compilation is most of the
    remaining in-transaction CPU), and batching `touch_session` (its cost is the commit, not work).
  - Not security-relevant: tests and the race run's build flags only, plus two auth paths that now hash before
    their transaction, with the same parameters and stored result.

### ADR-0063 — SpaceMan admin MCP: SpaceMan's own API as an MCP server for a space's runs, through a secretless internal token (security-relevant change)
- **Status:** **Accepted**, 2026-11-08 (proposed 2026-11-07). The operator's decisions are recorded with the design;
  question numbers (Q1–Q9) refer to the questions of the admin MCP plan. The operator's decisions of 2026-11-08 on the
  security review (SR-MCP-01) and the final review (F1, F2, F6) are listed under §Operator decisions on the reviews
  below; none of them changes behaviour.
  Implemented so far: migration 0044, the internal token and its principal, the audit attribution and the approver
  exclusion (AM-2); the tool catalog, classification and in-process executor (AM-3, AM-4); the gateway kind with its
  create, edit and re-issue operations, `approvals.Require` and the wiring in central (AM-5); the CLI (AM-6); the
  external endpoint (AM-10, §External endpoint below); the web (AM-7); the end-to-end test over the run bridge, the
  CHANGELOG and the security review entry (AM-8). The Terraform resource (AM-9) is in terraform-provider-spaceman
  PR #12. Requirements SM-MCP-005..009; threats T-33, T-38.
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
       owner loses. Revoking the token (Tokens page, or revoking its parent), disabling the owner, deleting the server
       (revokes the token in the same transaction, `auth.RevokeAPITokenTx`), disabling the server (its tools are hidden
       and calls refused; the token stays, so re-enabling needs no re-issue), revoking the grant or reaching the expiry
       stops it on the next call.
     - A child token never outlives its parent: when a token creates the server, the internal token's expiry is capped
       at that token's (a "never" request becomes the parent's expiry). Revocation also cascades from parent to child.
     - "Re-issue" revokes the token and mints a new one in one transaction: for grant changes, owner transfer and expiry
       renewal.
  3. **Audit.** Calls are attributed to the owner, with `via: spaceman_mcp:<token id>` and `run: <run id>`
     (`audit.Actor.Run`, `api.Actor`). The gateway's `mcp_server.tool_called` event also gains the operation and class
     (AM-5).
  4. **Never an approver.** The approver re-check (SM-APPR-003) refuses any `spaceman_mcp:` actor and any internal
     token named as an API token. (A decision's stored actor carries no run, so the check keys on `via`.) The approval
     operations themselves are excluded (below).
  5. **Tool surface: one tool per API operation** (operator decision on plan question Q8), generated from the build's in-memory API
     document and the `Rule` table, named `<slug>__<operation_id>`. A reviewed classification puts each operation in
     `read`, `write`, `dangerous`, `sensitive-read` or `excluded`, and `TestEveryOperationIsClassified` keeps it
     complete (AM-3). Calls run through the API's own handler chain (authorization, validation, idempotency, audit)
     with the internal principal (AM-4).
     - Presets (`internal/spacemanmcp`):
       - `read`: read tools, viewer grants on one space;
       - `operate`: read and write tools, operator grants on one space;
       - `admin`: the default, labelled "infra". Read, write **and dangerous** tools, with `admin@*` (operator
         decision: full admin, with dangerous operations available and each one approved by a person);
       - `custom`: only the tools named.
     - `read` and `operate` add dangerous tools only by name. No preset includes sensitive-read tools; a server adds
       them by name. Every dangerous tool is flagged approval-required whatever the preset (`Tool.ApprovalRequired`,
       enforced in AM-5).
     - The catalog also hides tools that the token's grants can never reach.
     - Calls run only with an internal token's principal: the in-process handler and the executor refuse any other
       (a bearer token's would skip step-up). Path values `.` and `..` are refused, and any status from 300 up is a tool
       error. A create's idempotency key needs the run.
     - Session-rule reads filtered by the principal are read tools. These operations allow "any signed-in principal,
       filtered by grants", such as `list-spaces`, event connections and templates, and attention; their rule carries
       `Filtered` (`api.FilteredRead`, AM-5). The API refuses internal principals on every other Session rule, so
       personal operations (sign-in, MFA, own session, own tokens and preferences) stay excluded.
     - Creates get an `Idempotency-Key` derived from the run, the token, the operation and the canonical arguments, so
       a retried call returns the first result. Problems become tool errors; results above the server's cap are
       replaced by their size and a note.
  6. **Hard exclusions (kept, operator decision).** Never exposed, whatever the server's settings:
     - every operation whose rule is Public, Session, UserOnly, Reauth or MFA (sign-in, own account, step-up, token
       creation, user writes, root token, break-glass). The API also refuses an internal principal on these rules
       directly, although bearer tokens skip step-up (`authorizeOps`);
     - approvals and approval-policy writes; responding to, assigning, moving or commenting on human actions;
     - takeover and terminal input;
     - operations that return a one-time secret (node enrollment);
     - sign-in provider writes;
     - SpaceMan MCP management (`create-spaceman-mcp`, `update-spaceman-mcp`, `reissue-spaceman-mcp`), token operations,
       and gateway operations that target a `spaceman` server: the gateway service refuses an actor acting through a
       SpaceMan MCP server on a `spaceman` server (grant, revoke, check, delete), and `update-mcp-server` refuses a
       `spaceman` server for everyone;
     - restore acknowledgement.
  7. **Defaults (operator decisions for a trusted infra space).**
     - **Preset: full admin on all spaces** (`admin@*`), labelled "infra" in the UI. Narrower presets (read-only or
       operate, one space or all) stay available.
     - **Grant: the control role** of the chosen space.
     - **Approvals: none by default, but dangerous tools always need one** (operator decision on plan question Q3): deletes and purges, retention runs,
       harness profile settings (T-32), diagnostic launches, SSH key generation and rotation, account and event
       connection secrets, prompt, skill, role template and human-type publication, space role saves, HTTP MCP server
       and grant writes, node state and updates, relocations, and launches of diagnostic runs (`launch_run` with
       `kind: diagnostic` is classed dangerous per call). `approvals.Require` gates them with a matching policy or,
       without one, a synthetic one-use policy (policy ID `required`). A stored `mcp.*` policy that matches replaces
       the synthetic one, so a policy with `uses` above 1 lets one approval cover repeats of the identical call (the
       digest binds the arguments); policies are written by people only. The approval operation is
       `mcp.<slug>.<class>.<tool>`, so a policy can name a class (`mcp.infra.write.*`).
       - **A diagnostic launch is gated on its resolved kind.** The gateway classes a call by its arguments, but
         `launch-run` may leave `kind` out and take it from the space role's template, which only the API resolves.
         So the API refuses a diagnostic launch by an internal principal unless the call's context carries the
         gateway's mark (`gateway.WithApprovedCall`, set only after `approvals.Require` succeeded for that exact
         dangerous `launch-run` call). A role-only diagnostic launch is refused with a hint to pass
         `kind: diagnostic`, which the gateway then gates. No other operation resolves a run's kind on the server:
         schedules and subscriptions create inbox items and tasks, and dream and KB runs have fixed kinds.
       - **Saving a space role is dangerous** (`save-space-role`). A space role picks the template (its kind),
         profile, permission mode and settings every launch by that role inherits, so a save can make a role
         diagnostic or widen its runs' access with no later launch naming it. All saves are gated, not only the
         widening ones: whether a template is diagnostic or a profile or setting widens access depends on other
         catalog entries that can change after the check, and a per-field comparison would be a second, weaker
         policy to keep in step. Deleting a space role was already dangerous.
       - **`launch-run` stays a write, also with overrides** (operator decision 2026-11-08, final-review F2). A
         launch may override the role's `settings.permission_mode` (still held to the installation's
         `harness.max_permission_mode` ceiling) or its `account_ref`. Unlike a role save, such an override affects
         only the run being launched, which the call names and the audit records; it can't make a later launch
         diagnostic or widen runs nobody asked for, and a diagnostic launch is still gated on its resolved kind. A
         role save is persistent and reaches every later launch by that role, so it is dangerous; a launch's
         override is not. Accepted residual risk under T-33.
       - **Who approves in-run calls** (operator decision 2026-11-08, final-review F1). An in-run call's approval is
         requested in the run's own space, so anyone with `approval:decide` there may approve it, also when the call
         acts on another space or on an installation-wide target (an `admin@*` server's dangerous tools). This is
         deliberate for a trusted infra space, whose operators are trusted with the server's reach; accepted residual
         risk under T-33.
       - A server's `approval` is `none` (default) or `writes` (every write tool needs
       an approval too). Without an approval gate, such calls are refused (fail closed). Human-action responses and
       approval decisions refuse an actor acting through a SpaceMan MCP server at the service level as well.
     - **Expiry: never** by default (operator decision on plan question Q5); 1–366 days on request.
     - Only control and worker runs get the tools; KB, dreamer and diagnostic runs never do.
  8. **External endpoint in this release** (operator decision on plan question Q9; AM-10, SM-MCP-009). Central also serves the tool surface at
     `/api/v1/mcp` for operators' own MCP clients, authenticated with a bearer API token (never an internal one), with
     the same classification, exclusions and approval rules. The rules are in §External endpoint below.
  9. **API (AM-5):** `create-spaceman-mcp` (`POST /mcp-servers/spaceman`), `update-spaceman-mcp`
     (`PUT /mcp-servers/{id}/spaceman`: tools, approval mode, limits) and `reissue-spaceman-mcp`
     (`POST /mcp-servers/{id}/reissue`: new grants, expiry or owner), all `StepUp(mcp:write)`. Servers carry `kind` and,
     for a spaceman server, its settings and token (ID, owner, grants, expiry, state). The run manifest's `mcp_servers`
     entries gain `kind`. A spaceman server can't take the slug `spaceman` (it would read as a built-in `spaceman_*`
     tool). The database requires a token exactly for the spaceman kind.
  10. **Terraform:** a new resource `spaceman_admin_mcp_server` (grants, preset, tools, approval, expiry; computed
      token ID, owner, expiry and effective tools; no secret attributes), granted with the existing `spaceman_mcp_grant`
      (AM-9).
  11. **Numbers:** migration 0044 (four `ADD COLUMN`s: `mcp_servers.kind`, `api_token_id`, `spaceman_json`;
      `api_tokens.purpose`, forward-only like every migration; numbered 0041 until the control reminders took that
      number, then 0042 until Harbor Pilot's usage migration took 0043), T-33, T-38 (external endpoint),
      SM-MCP-005..009. No protocol minor.
- **External endpoint (AM-10, SM-MCP-009, threat T-38).** `/api/v1/mcp` (`api.MCPPath`) serves the same catalog and
  executor over streamable HTTP to an operator's own MCP client (`internal/spacemanmcp` `Endpoint`). It is not an API
  operation: it is absent from the OpenAPI document and the CLI.
  - **On by default, no switch** (operator decision 2026-11-08, final-review F6). Every user API token can use the
    endpoint, within its own grants; there is no configuration setting or per-token flag to turn it off. A token
    already reaches the same operations on the API, so the endpoint adds no authority; an operator who doesn't want
    it can keep `/api/v1/mcp` out of the reverse proxy. Accepted with T-38.
  - **Who can call it:** a user's bearer API token, acting as itself (token **and** owner, as on the API). The guard
    (`api.Deps.mcpEndpoint`) refuses a session cookie and the root token (403). Internal tokens
    (`purpose = 'spaceman_mcp'`) never authenticate as bearers (`AuthenticateAPIToken` refuses any purpose, 401), and
    the guard refuses a principal with a purpose as well. The guard passes on a copy of the principal marked
    `MCPEndpoint`; only the guard sets it.
  - **Same rails as the in-run tools** (`auth.Principal.ViaMCP` covers both): the API refuses Session (unless
    `Filtered`), UserOnly, Reauth and MFA rules for such a principal, although bearer tokens otherwise skip step-up;
    the hard exclusions keep the same operations out of the catalog; the in-process handler and the executor accept
    this principal (and an internal one) only. Human-action responses and approval decisions refuse an actor on the
    `mcp_endpoint` channel at the service level (`audit.Actor.MCP`), although the catalog never offers them. As defence
    in depth the other service-level refusals written for internal tokens cover the endpoint too: issuing API or
    internal tokens (`Principal.ViaMCP`), and creating, editing, re-issuing, granting, checking or deleting a SpaceMan
    MCP server (`audit.Actor.MCP`); none of these is in the endpoint's catalog.
  - **Tool selection: the token's grants as they are.** The endpoint offers the `admin` preset's classes (read, write,
    dangerous) filtered by what the token's grants can reach. Of the two options (a preset stored on the token, or the
    grants as they are), this is the simpler one: it needs no migration, no token setting, no UI or Terraform attribute,
    and the grants are already the scope the operator chose for that token; a narrower client gets a narrower token.
    Two kinds of tool are left out: sensitive reads (recordings, transcripts), which no preset includes; and dangerous
    tools that act on no space (accounts, Git SSH keys, HTTP MCP servers and grants, harness profiles, role templates,
    publishing, installation-wide human policies, node writes), because an approval belongs to a space. Those stay on
    the API, the CLI and the web UI.
  - **Approvals: dangerous calls always need one, decided by another person.** A dangerous tool, and a `launch_run`
    asking for `kind: diagnostic`, goes through `approvals.RequireFor` with the operation
    `mcp.endpoint.dangerous.<tool>`, the resource `api_token:<token id>` and the requester key `api_token:<token id>`
    (role `mcp_endpoint`): only calls with the same token and the identical arguments can use the approval. A stored
    policy for that operation sets the uses and lifetimes; without one a synthetic one-use policy does (one hour after
    the decision, a day to decide). Before asking, the endpoint checks that the token may use the tool in that space,
    so nobody is asked about a call the API would refuse. Read and write calls run at once, as the same token's calls
    on the API would.
    - **Rule: the token's own user never approves it.** Not through the token, not in a browser session, not with
      another of their tokens: `onFinalized` refuses an approving decision when the deciding user owns the requesting
      token, and the gate revokes an approval that such a user decided anyway. The token acts for its user, so the user
      approving their own client's call adds no second check; a stolen token plus that user's session would otherwise
      approve itself. That user may still **deny** or **withdraw** (cancel) their own client's pending request from
      a session or the API (not through the MCP channel, which never decides): that only stops the call. Consequence:
      an installation with one administrator can't run dangerous tools through the endpoint with a user's decision;
      that person uses the API, CLI or web UI for them.
    - **The root token may approve a token's request** (operator decision 2026-11-08, SR-MCP-01: kept). The rule
      above matches user decisions only (`audit.ActorUser`), so an approval decided with the root token is accepted.
      This is the documented path for a single-administrator installation, where no second user exists to approve the
      endpoint's dangerous calls. The root token is itself refused on the endpoint, so it can't make the call it
      approves.
  - **Diagnostic launch gate:** `authorizeLaunchKind` refuses a diagnostic launch by any `ViaMCP` principal unless the
    call carries `gateway.WithApprovedCall("launch-run")`, which the endpoint sets only after `RequireFor` succeeded
    for that call. A launch by a space role whose template is diagnostic, without `kind`, is refused with the hint.
  - **Edge protection:** the API's own middleware (security headers, request IDs, the body limit
    `http.max_body_bytes`, the failed-bearer limit of 20 per minute per address) and, per token, 5 requests per second
    with bursts of 50 (429). The limit counts calls because a request carries exactly one JSON-RPC message: a batch
    (a body whose first non-space byte is `[`) is refused with 400 before any message in it runs, and so is an
    `Mcp-Protocol-Version` older than 2025-06-18 (the version that dropped batching; the go SDK accepts batches for
    older or missing versions). A request without the header is accepted, as a client's first request may not send
    it. A request with an `Origin` or `Sec-Fetch-*` header (a browser) is refused, and no CORS
    headers are ever sent. The MCP SDK's localhost DNS-rebinding check is off: central normally sits behind a reverse
    proxy on the same host, and rebinding needs a browser, which is refused, plus a bearer token, which a page doesn't
    have. The server is stateless (no `Mcp-Session-Id`; GET and DELETE answer 405) and answers in JSON.
  - **No derived idempotency key.** A run's retries are deduplicated (decision 5); on the endpoint a person drives the
    client and a repeated create is meant, as with a CLI command without `--idempotency-key`.
  - **Audit:** every operation is attributed to the token's user with `via: api_token:<id>` and `channel:
    mcp_endpoint` (`audit.Actor.Channel`). Each tool call is also audited as `mcp_endpoint.tool_called` (target the
    token; tool, operation, class, result, duration, and the space of a dangerous call; never arguments or results).
    Approval requests and uses carry the same actor.
- **Alternatives:**
  - **A first-class service principal** (`svc:<id>`), independent of any person. Access would survive admin turnover
    without a re-issue, but it roughly doubles the authentication work (new subject kind, grants, management, audit) and
    adds an identity nobody is accountable for. Rejected for now (plan question Q1).
  - **Register central's own HTTP API as an ordinary `http` server**, with a bearer token in its sealed headers. It
    needs a private-address exemption for loopback, keeps a real admin secret at rest, and adds a network hop to itself.
  - **A node-side binary with the token in the run's secrets file.** It puts an administrator secret within the agent's
    reach on the node, needs a protocol minor and collides with Harbor Pilot's secrets delivery (ADR-0060).
  - **Three meta-tools** (search, describe, call) instead of one tool per operation: smaller catalogs, but every call
    is opaque to approval policies and to the harness's own tool permissions. Revisit if `admin@*` (about 150 tools)
    strains a harness's context (plan question Q8).
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
  - The external endpoint gives an operator's MCP client what the operator's token can do on the API, short of the
    hard exclusions and with dangerous calls approved by someone else or the root token (T-38).
- **Operator decisions on the reviews (2026-11-08).** All four keep the behaviour as built:
  1. **Root approval kept (SR-MCP-01).** The root token may approve dangerous calls made through `/api/v1/mcp`; this
     is the documented path for single-administrator installations (§External endpoint).
  2. **F1, accepted as is.** In-run dangerous calls are approved in the run's own space: any operator there with
     `approval:decide` may approve admin-scope calls (other spaces, installation-wide targets) made by the infra
     space's SpaceMan MCP server. Deliberate for the trusted infra space; part of T-33's accepted risk (decision 7).
  3. **F2, accepted as is.** `launch-run` stays a write, also when it overrides `settings.permission_mode` (up to the
     installation ceiling) or `account_ref`; diagnostic launches stay gated. Accepted residual risk under T-33; the
     reasoning beside `save-space-role` (dangerous) is in decision 7.
  4. **F6, accepted as is.** `/api/v1/mcp` is on by default for every user API token, with no switch (§External
     endpoint; T-38).

### ADR-0064 — Scrolling back through run terminals: Claude Code's classic renderer by default, scrolling handled by the web view
- **Status:** Accepted, 2026-11-07 (operator request: scroll the terminal without taking it over; follow-up bug: in a
  takeover the wheel showed "Scroll wheel is sending arrow keys · use PgUp/PgDn to scroll").
- **Context:**
  - Without a `tui` setting, Claude Code 2.1.317 chooses its renderer itself. It picks the full-screen one for a recent
    install (`fresh_install_on`, once `.claude.json` has a `firstStartVersion`, which every launch after an instance's
    first has) or when a server-side feature flag says so. The operator's runs used it. That renderer draws in the alternate screen with mouse tracking (DEC modes
    1000/1002/1003, SGR 1006) and keeps its own virtualized scrollback. The terminal itself never holds earlier output:
    the recording, the live stream and the browser's terminal have only the current screen.
  - A watch-only viewer sends no input (SM-UI-004), so they can't ask the program to scroll. The operator had to switch
    to Replay to read a long reply, and Replay shows the same screens.
  - ghostty-web 0.4.0 turns every wheel event in an alternate screen into `ESC[A`/`ESC[B` arrow keys, whatever mouse
    mode the program asked for. In watch mode nothing listens, so the wheel did nothing. During a takeover the arrows
    reached Claude Code, which answers them with the warning above instead of scrolling.
  - On a normal screen ghostty-web keeps a 10,000-line scrollback, but it jumps to the bottom on every write, so a
    viewer reading earlier output was pulled back about once a second.
- **Decision:**
  - **Claude Code runs get `"tui": "default"`** (the classic renderer) in the run's `settings.json`, unless the
    profile's Claude Code settings (ADR-0059) set `tui`. The classic renderer writes to the normal screen, so the
    output goes into the terminal's scrollback, the recording and the live stream. A profile that sets
    `"tui": "fullscreen"` keeps the full-screen renderer and accepts that watchers can't scroll back. Verified in the
    2.1.317 binary: the `tui` setting is checked before the fresh-install default (`settings_off`).
  - **The node applies the default**, where it writes the run's `settings.json` (`internal/harness/claude.go`), not
    central. It therefore needs the agent from SpaceMan 0.1.0 or later; a run on an older node keeps Claude
    Code's own choice of renderer. The resolved `claude_settings` that central's API returns are the profile's own and
    don't show the default.
  - **The web view handles the wheel and touch itself** (`web/src/features/terminal/scroll.ts`). Scrolling up moves
    the box around the terminal first, then the scrollback; scrolling down goes back the same way. In an alternate
    screen, a watch-only viewer sends nothing. A takeover holder sends what a terminal would: SGR (or X10) mouse-wheel
    reports while the program tracks the mouse, arrow keys only under xterm's alternate-scroll mode 1007 (`ESC O A`/
    `ESC O B` when the program set application cursor keys, DECCKM; `ESC [ A`/`ESC [ B` otherwise), else nothing.
    ghostty-web's arrow keys never reach anyone.
  - The view keeps a scrolled-up viewer on the same lines while output arrives, and shows **Jump to live** until they
    return to live output.
- **Alternatives:**
  - **(a) A client-side scrollback rebuilt from the stream:** a full-screen renderer repaints its screen in place, so
    lines that scroll out of its view never pass through the terminal as scrolled-off lines. Rebuilding them means
    guessing from screen diffs, and the guesses would be wrong for any redraw that isn't a plain scroll.
  - **(b) A server-side scrollback snapshot:** the node has the same screens as the browser; there is nothing more to
    snapshot.
  - **(c) A "recent output" pane fed by the transcript:** it would show the conversation, not the terminal, and needs
    its own view and its own access rules for transcripts in the terminal page.
  - **`CLAUDE_CODE_DISABLE_ALTERNATE_SCREEN=1` in the run's environment:** it has the same effect, but a profile
    couldn't opt back in without a new setting. `tui` already is one.
- **Consequences:**
  - Watchers can scroll back through everything a Claude Code run printed, up to the browser terminal's 10,000-line
    scrollback, on desktop and on a phone (touch). No input is sent.
  - Claude Code's classic renderer repaints its input area on the normal screen. The recording is somewhat larger than
    with the full-screen renderer and the screen may flicker briefly during a repaint. Runs started before this change
    keep the renderer they started with until they are relaunched.
  - The `tui` key is SpaceMan's in the run's `settings.json` (ADR-0059): a `/tui fullscreen` typed during a takeover is
    undone at the next launch. Set it in the profile instead.
  - Other full-screen programs (and Claude Code with `"tui": "fullscreen"`) still can't be scrolled back while
    watching; in a takeover the wheel scrolls them the way it would in a desktop terminal (LIMITATIONS, Web UI).
  - Not security-relevant: watch mode still sends no input, and a takeover holder's wheel reports go through the same
    audited takeover input as their keys.

### ADR-0065 — Control schedules reminders to itself: scheduled messages to control
- **Status:** Accepted, 2026-11-07 (operator request: control asks to be woken later, e.g. in 6 h, when the inbox and
  queue are empty and it has stopped).
- **Context:**
  - With control auto-start and idle stop (ADR-0056), an idle control stops and is started again only for waiting work:
    a stored message to control, or a pending human response that wakes it. Control had no way to have itself woken
    later to check on something nobody will report, such as a deploy, a slow review or an answer that should arrive.
  - The `schedule` package runs recurring schedules (cron with missed-run policies) that create inbox items. A
    one-off, agent-set time doesn't fit it, and an inbox item would be work for people too.
  - Messages already carry everything else: the dedup key, audit, the notifier's terminal notice, the "a new control is
    told once" path (SM-COMM-003), and the auto-start sweep (`ListSpacesWantingControl`).
- **Decision:**
  - **A scheduled message is a message to `control` with `deliver_at`** (migration 0041, a new nullable column with a
    partial index on pending ones). `spaceman_message_send` takes `deliver_at` (RFC 3339) or `delay` (a Go duration),
    from 1 minute to 30 days ahead. Only control runs may schedule (`forbidden` otherwise), only `to: control`, and not
    replies. Operators don't schedule. The API's send operation is unchanged.
  - **Until it is due, it does not exist for delivery.** `ListMessagesFor` (fetch, notices, `Waiting` for idle stop)
    and `ListSpacesWantingControl` (auto-start) skip messages whose `deliver_at` is in the future. A pending reminder
    therefore doesn't keep control running and doesn't start it.
  - **When it is due, it is an ordinary stored message to control.** A running idle control gets the usual notice; with
    none running, the notifier's sweep asks the supervisor to start control within `control_autostart` and its hourly
    cap; a control started later is told about it once. A run never receives its own messages, *except* a scheduled
    one: the control that set a reminder and is still running receives it. `deliver_at` stays set after delivery, so
    the UI keeps showing "scheduled for".
  - **The notifier wakes for the earliest due reminder** (`NextScheduledMessage`), instead of waiting up to its
    one-minute sweep. Due times come from `messages.Service.Clock` (default: the store's clock), so tests inject a
    fake clock.
  - **Limits:** at most 50 pending per space (`rate_limited`). A `dedup_key` on a scheduled send returns the pending
    reminder with that key from any sender, so a later control run re-scheduling is idempotent. A key the sender
    already used for a message that is not a pending reminder (one already delivered, or a plain message) is a
    `conflict`, so a reminder is never silently dropped. Moving one means cancel and send again.
  - **Listing and cancelling:** control has `spaceman_message_scheduled` and `spaceman_message_cancel`. Operators use
    `GET …/messages?scheduled=true` and `POST …/messages/{id}/cancel` (`message:write`), and the Messages page's
    Scheduled view. A cancelled reminder is resolved as "cancelled before delivery". Audit: `message.sent` carries
    `deliver_at`, and a cancel is `message.cancelled`.
- **Alternatives:**
  - **A future `available_at`:** no migration, but `available_at` is overwritten at delivery, so nothing would mark the
    message as scheduled afterwards, and the control that set it would never receive it.
  - **One-shot schedules in the `schedule` package:** they create inbox items for everyone rather than a note for
    control, and they need a second listing and cancelling surface.
  - **Deferred tasks (`spaceman_task_create` with `not_before`):** the request asks for a wake-up rather than queued
    work. Recorded as a possible follow-up.
- **Consequences:** the change adds no new authority. Scheduling is limited to control writing to control, the
  existing message permissions cover the operator operations, and the content stays untrusted data that is never
  typed into a terminal. The limits on reminders (50 pending, 30 days, auto-start's hourly cap) mean a control can't
  wake itself in a tight loop.

### ADR-0066 — Agents ask people only through human actions: Claude Code's terminal question picker and plan mode denied by default
- **Status:** Accepted, 2026-11-08 (operator request: a control run asked the operator through Claude Code's
  AskUserQuestion picker in its terminal and then waited there; "we should somehow limit agents from doing this").
- **Context:**
  - Claude Code's `AskUserQuestion` tool draws a picker in the run's terminal and waits for an answer typed there.
    SpaceMan runs are unattended: nobody may be watching the terminal, nothing reminds anyone, and the run stalls
    silently. It also holds its slot and its idle-stop timer.
  - `spaceman_human_ask` (SM-HUM-008) does the same job durably: the action is listed under Needs you, people are
    reminded per policy, and the answer wakes the agent (SM-HUM-009).
  - Claude Code drops a tool that a whole-tool `permissions.deny` rule names from the tools it sends to the model
    (`filterToolsByDenyRules`). Verified with Claude Code 2.1.317 (2026-11-08, throwaway config dir, a local fake API
    that recorded each request's tool list): an interactive session offered `AskUserQuestion` and `EnterPlanMode` in
    every request without the rule, and neither with it. In print mode Claude Code doesn't offer `AskUserQuestion` at
    all. The operator checked the same on Claude Code 2.1.319: a denied tool isn't offered to the model.
  - `EnterPlanMode` switches the run into plan mode, which ends in `ExitPlanMode`'s plan-approval dialog in the
    terminal. An unattended run waits there too.
- **Decision:**
  - **Every Claude Code run SpaceMan launches** (control, worker, KB, dreamer, diagnostic) gets `AskUserQuestion` and
    `EnterPlanMode` in `permissions.deny` of the run's own `settings.json`. The model never sees the tools, so it never
    tries them.
  - **Merge, never replace.** The adapter appends them to the profile's own `permissions.deny` (ADR-0059): the profile's
    rules come first, in their order, duplicates are dropped (profiles that already deny these tools get each once),
    and the other permission keys (`allow`, `ask`, `additionalDirectories`, `defaultMode`) are kept as they are. The
    profile's stored settings are not changed.
  - **Opt-in, per tool:** a profile that lists one of them in its own `permissions.allow` gets no deny for it (a deny
    would win over its allow). That is the only way back; the default is not a setting.
  - **Prompts:** SpaceMan's own orientation (the first section of every run's system text) and the starter role prompts
    say "Never ask through an interactive terminal prompt; use spaceman_human_ask". The orientation reaches existing
    installations; starter prompts already seeded are not rewritten (ADR-0051).
  - **Codex:** codex-cli 0.166.0 has its own question tool, `request_user_input`, offered in Plan mode and in Default
    mode only with a feature flag (`default_mode_request_user_input`); found in the binary's strings, not run. Runs start
    in Default mode, and the orientation tells Codex agents the same. SpaceMan sets no override for it yet (unverified
    against a live session; LIMITATIONS).
- **Alternatives:**
  - **`--disallowedTools AskUserQuestion` on the command line:** the same effect, but a profile could not opt back in,
    and the run's settings would not show it.
  - **Prompt guidance only:** the operator's run already had human actions and used the picker anyway.
  - **Detecting a waiting picker and raising an attention item:** useful for prompts SpaceMan can't deny (a harness's
    own permission dialog, Codex), but it needs a reliable "waiting for input" signal from the terminal. A follow-up
    (LIMITATIONS).
- **Consequences:** no new authority, no protocol change. The deny is written by the node when it builds the launch,
  so it takes nodes on this release or later; runs on older nodes still see the tool, and the profile's resolved
  settings in the API don't show the deny. Claude Code's own permission dialogs are unchanged: they follow the
  permission mode, and `skipAutoPermissionPrompt` only skips the one-time notice shown when auto mode starts (auto mode
  still asks for `ask` rules, reads outside the working directories, and when its classifier falls back after repeated
  blocks). A built-in PermissionRequest hook for those is a separate follow-up (since done: ADR-0068). A run whose permission mode is `plan`
  (a profile setting) still starts in plan mode and ends in the `ExitPlanMode` dialog.

### ADR-0067 — The System page: an administrator's view of the instance; the version stays public
- **Status:** Accepted, 2026-11-08 (operator request for v0.1.0: a System page like Hallpass's, and a version API).
- **Context:**
  - Hallpass's System page shows lockdown, storage, break-glass, version, boot ID, policy version and live counts;
    Harbor Pilot's "This instance" shows version, public URL, start time, data directory, key ID and schema versions.
    SpaceMan had the pieces (build info, readiness checks, the attention summary's storage section, `/system/info`) but
    no single view of the instance.
  - The public, unauthenticated `get-system-info` (`GET /system/info`) has returned the version since M1: the
    sign-in and setup pages need it before anyone signs in.
- **Decision:**
  - **`get-system` (`GET /system`) needs `system:read`**, which only administrators (and the root token) hold. It
    returns build info (version, commit, build date, Go version, platform), the start time and uptime, a **boot ID**
    (`boot_…`, new at every start of central; it is never persisted), the applied and newest database migration, the
    node protocol range (major, min and max minor), the attention summary's storage section plus the database and WAL
    sizes, and counts: spaces not archived, active and connected nodes, runs starting/running/stopping, open human
    actions and connected live-event streams. Counts come from one SQL query; nodes from the fleet's live connection
    state; migration 0045 adds a partial index for the running-runs count. A section whose source fails (the schema
    version, the counts query, the node state) is reported as `unavailable` instead of failing the whole response.
    Central logs the boot ID in its `spaceman serving` line.
  - **No secrets:** no key material, no KEK source or path, no keyring IDs, no tokens. The public URL and the data
    directory path are shown, because only administrators can read the endpoint.
  - **The version alone stays public** in `get-system-info`, as before: it is not a relaxation, and the web UI shows it
    to every signed-in user in the account menu. Hiding it would not hide much (the OpenAPI document carries it too),
    and the sign-in page needs it.
  - The admin MCP classifies `get-system` as a read. CLI: `spaceman system status`. Web: **System** under Access,
    shown to global administrators.
- **Alternatives:** extending `get-system-info` with an authenticated detail section (one operation with two shapes
  for two audiences, hard to describe in OpenAPI); serving the detail on the management listener (unauthenticated,
  private address: no UI, and it would expose the data directory to anything that reaches it).
- **Consequences:** a new admin-only read with no new authority. The boot ID lets an operator tell a restart from a
  long-running instance and match logs to a start.

### ADR-0068 — No permission dialogs in SpaceMan's Claude Code runs: a deny-only PermissionRequest hook and the run's own space folder
- **Status:** Accepted, 2026-11-08 (operator request for v0.1.0). **Security-relevant change:** each run's file tools may
  now read and write its own space folder without asking (they previously stopped at a dialog), and every permission
  dialog is answered with a deny.
- **Context:**
  - Every SpaceMan run is unattended, so a Claude Code permission dialog stalls it: nobody types an answer into its
    terminal, nothing reminds anyone, and the run holds its slot.
  - In auto mode, Claude Code still shows a dialog in three cases: for an `ask` rule, for a read outside the working
    directories, and when its classifier falls back to asking after repeated blocks (3 in a row or 20 in total).
    `skipAutoPermissionPrompt` doesn't help with any of them: it only skips the one-time notice shown when auto mode
    starts.
  - Claude Code runs a `PermissionRequest` hook before it shows a dialog. Its input carries `hook_event_name`,
    `tool_name`, `tool_input` and `permission_suggestions`; its output
    `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"…"}}}` denies
    the request and hands the message to the model as the tool's result (`interrupt` unset, so the turn continues). In
    auto mode a hook can't approve anything (Claude Code ignores an allow there). Checked in Claude Code 2.1.317's
    hook schema and live (docs/contracts/harnesses.md).
  - Runs start in a checkout (`worktrees/…`) or in `work/`, so reading the space's own files (plans and notes under
    `work/`, other checkouts) was a read outside the working directories, and so a dialog.
  - The operator worked around both through the profiles' `claude_settings` (a PermissionRequest hook script and the
    node's whole `spaces/` folder as an additional directory). The product should do this itself, and more narrowly.
- **Decision:**
  - **Every Claude Code run SpaceMan launches** (control, worker, KB, dreamer, diagnostic) gets a `hooks.PermissionRequest`
    entry (`matcher` `*`) in its own `settings.json`. The entry runs `'<agent binary>' hook permission-request`, a
    subcommand of the node agent binary, so nothing extra has to be installed on the node.
  - **The hook only denies.** Whatever its input (none, malformed, any event), it writes a deny whose message says the
    run is unattended, not to retry or work around it, and to ask a person with `spaceman_human_ask` or tell its control
    with `spaceman_message_send`. It names the tool only when the name looks like a tool name. It can never answer
    "allow", so it can't widen what a run may do. It reads nothing but its input and needs no configuration.
  - **The run's own space folder** (`<data_dir>/spaces/<space-dir>`, its real path) is added to
    `permissions.additionalDirectories`, and only that folder, never the node's whole `spaces/` folder.
  - **Its login files are denied.** The space folder also holds the instances' harness config dirs, which can hold a
    Claude Code login (`instances/*/claude/.credentials.json`) or a Codex login (`instances/*/codex/auth.json`). Those
    two paths get `Read` and `Edit` deny rules (verified: such a read fails with "denied by your permission settings").
  - **Other instances and every instance's harness configuration are not writable** (review fix M1). Adding the space
    folder let a run's file tools write other instances' `instances/*/claude/settings.json` with no dialog and no
    classifier (verified live with Claude Code 2.1.317 in `acceptEdits`), and a key planted there (`disableAllHooks`,
    `env.ANTHROPIC_BASE_URL`, `apiKeyHelper`, …) took effect at that instance's next launch. Now:
    - every instance folder other than the run's own that exists at launch gets `Edit(//<space>/instances/<id>/**)`,
      up to 50 of them. With more, a run gets none of them, so the settings stay bounded, and only the next measures
      apply;
    - every instance, the run's own included, gets `Edit` deny rules (`harness.ClaudeSpaceProtectedFiles`) for
      everything in its config dir that Claude Code loads, runs or restores at a later launch or resume, whatever the
      number of instances. These also cover an instance created after the launch. They, and the login-file rules, are
      anchored on the node's spaces folder (`Edit(//<data_dir>/spaces/*/instances/*/claude/...)`), so they cover every
      space's instances: a profile may add the whole spaces folder as a directory (operators did, before this ADR), and
      another space's instance configuration must not be writable then either (round-2 re-review, verified live). The list mirrors the entries Claude
      Code 2.1.317 protects in its own config dir, for every instance: `claude/settings.json`, `settings.local.json`,
      the keys record, `.claude.json`, the cached `remote-settings.json*` and `policy-limits.json*`, the instructions
      (`CLAUDE.md`, `rules/`, `agents/`, `commands/`, `skills/`, `output-styles/`, `workflows/`, `routines/`,
      `loop.md`), `plugins/`, `cowork_plugins/`, `hooks/`, `scheduled_tasks.json`, `launch.json`, `daemon.json`,
      `daemon/`, `shell-snapshots/` and `session-env/` (sourced by its Bash tool), `projects/` (auto memory and
      transcripts), `agent-memory/`, `backups/`, `tasks/`, `teams/`, `todos/`, `file-history/`, and all of `codex/`
      (review round 2: F1 added `rules/`, F2 `projects/` and the rest, F3 the cached remote and policy settings);
    - **the run's own auto memory is moved out of the space folder** (round-2 F2). Claude Code loads an instance's auto
      memory (`MEMORY.md`) into its next run, so writing another instance's memory was persistent prompt injection: a
      worker reading untrusted text could poison control. The node sets `autoMemoryDirectory` (a key only the profile
      or SpaceMan may set) to `<data_dir>/automemory/<space-id>/<instance>/projects/<project key>/memory`, outside the
      space folder and every glob a run can write. Claude Code lets the run's file tools write and read its own auto
      memory folder there without a dialog, and loads `MEMORY.md` from it (verified live with Claude Code 2.1.317).
      The project key is Claude Code's own (the main checkout for a linked worktree; non-alphanumerics become `-`). At
      the instance's first launch on this release the node copies the memory it kept in its config dir
      (`claude/projects/*/memory`) there under the same keys, folders and regular files only (never symbolic links),
      and marks the folder done only after a full copy; the old copy stays. A partly copied file never stays: each file is
      written to a temporary name and renamed into place. A profile's own `autoMemoryDirectory` is ignored, with a
      warning in the node's log: a fixed folder shared by the profile's instances would bring cross-instance memory
      poisoning back. A relocation's release retains or deletes `automemory/<space-id>` with the space folder;
    - the rule is not the simpler `Edit(//<space>/instances/**)`: the run's own file tools still write its plan files
      (`plans/`, plan mode) and background job folder (`jobs/`) in its config dir, a deny rule wins over Claude Code's
      carve-out for them (verified), and permission-rule globs have no "all but one" form (`[!x]` is not negation
      there). No `Read` deny is added for `instances/**`: Claude Code reads its own tool results there.
    - A space folder that holds a glob character (`* ? [ ] { } \ !`) gets no directory and no rules, since the rules
      could not be written exactly (N2). A folder under `instances/` named with one is not an instance (SpaceMan's IDs
      never hold one) and is skipped.
  - **Defence in depth: the node strips dangerous settings keys at every launch.** Before the harness starts, the node
    removes from the instance's `settings.json` every top-level key that runs commands, adds credentials or
    environment, or changes what hooks, plugins, the sandbox or the classifier allow, unless the profile or SpaceMan
    set it (`harness.ClaudeUnsafeSettingsKeys`: `disableAllHooks`, `hooks`, `env`, `*Helper`, `*Helpers`,
    `awsAuthRefresh`, `awsCredentialExport`, `gcpAuthRefresh`, `processWrapper`, `statusLine`, `subagentStatusLine`,
    `fileSuggestion`, `sshHostCommands`, `permissions`, `autoMode`, `sandbox`, the plugin and project-MCP keys,
    `allowedHttpHookUrls`, `httpHookAllowedEnvVars`, `autoMemoryDirectory`). Keys Claude Code keeps there itself
    (`feedbackSurveyState`, `model` and the like) stay. A removal is logged with the key names, never the values. It
    doesn't depend on the keys record, so settings planted in a new instance's folder before its first launch are
    stripped at that launch too.
  - **The hook fails closed** (review fix S1). The command is
    `'<agent>' hook permission-request 2>/dev/null || printf '%s' '<fixed deny>'`. When the binary is missing, crashes or
    exits non-zero, the run still gets a deny, not a dialog: Claude Code shows the dialog for a failed hook, and exit
    code 2 doesn't deny this event. A hook that runs past its 30 s timeout still ends at the dialog.
  - **Merge, never replace** (as in ADR-0066). The profile's own hooks of every event come first, in their order, and
    SpaceMan's entry is appended to `PermissionRequest` (and not added again when the profile already has it). The
    profile's directories and deny rules come first, and duplicates are dropped. The profile's stored settings are not
    changed. A `hooks` value that isn't valid Claude Code settings (not an object, or a `PermissionRequest` value that
    isn't a list) is replaced.
- **Alternatives:**
  - **Prompt guidance only:** the dialog comes from Claude Code, not the model; the model can't avoid an `ask` rule.
  - **A hook that allows** (approving the dialog's request): it would turn every `ask` rule into an allow and bypass
    the classifier's fallback. Approval stays with people, through human actions.
  - **The whole `spaces/` folder** (the operator's workaround): it would let one space's runs read every other space.
  - **Turning off `permissions.blockReadsOutsideWorkingDirectories` or the sandbox:** much wider than one folder.
  - **A shell script shipped next to the binary:** another file to install and keep in step.
- **Consequences:**
  - A run never waits at a Claude Code permission dialog. A denied request reaches the model as a tool error that
    names `spaceman_human_ask`.
  - What was "ask a person in the terminal" (who was usually not there) is now "denied". An `ask` rule therefore
    works like a deny in SpaceMan runs; a profile that wants the call to happen must allow it.
  - **What a run's file tools reach.** They reach the run's own space without a dialog. They can read and write the
    space's checkouts and `work/`. They can read other instances' transcripts (`projects/*.jsonl`), settings and
    memory, but not the two login files. They can't write other instances' folders, or any instance's harness
    configuration, instructions, memory or transcripts (M1 above). They write their own auto memory outside the space
    folder. This is wider than before. The shell runs as the same OS user, but every shell command
    goes through auto mode's classifier (or a dialog in other modes), so the shell did not already reach these files
    without a check. File tools inside a working directory skip both.
  - **Accepted residuals.**
    - Reading other instances' transcripts and memory.
    - Writing what Claude Code doesn't load at launch in the folder of an instance created after the run launched, or
      of every other instance in a space with more than 50 of them: plan files, background job output and the like.
      Their settings, instructions, memory and transcripts are never writable this way (round 2 closed cross-instance
      memory poisoning).
    - The protected list and `ClaudeUnsafeSettingsKeys` are deny-lists checked against Claude Code 2.1.317: an entry or
      key a later Claude Code version adds is not covered until it is added (F4; an allow-list is a follow-up in
      PLAN.md).
  - **Hook precedence and profile hooks** (S2). A profile that sets `disableAllHooks` turns SpaceMan's hook off too.
    When the profile has its own `PermissionRequest` hook, Claude Code runs both. Outside auto mode the first answer
    wins, so a profile hook that allows can win over SpaceMan's deny. In auto mode Claude Code ignores any hook's
    allow. A profile editor is trusted with this (T-32).
  - The node writes both when it builds the launch, so they take nodes on this release or later. The profile's
    resolved settings in the API don't show them. Codex is unchanged.

### ADR-0069 — Tiered local gates, a one-run coverage floor and cached CI tools
- **Status:** Accepted, 2026-11-08 (gate wait-time work, subtasks T1, T2, T10, and T7 and T8 below). Not a relaxation: every check in
  `make check` and in CI still runs. The coverage floor applies to the same packages with the same rules.
- **Context:**
  - Agents spent much of their time waiting for `make check` (about 13.5 minutes on a 32-core node). It ran a median
    of twice and up to nine times per task.
  - The coverage floor ran 28 packages one `go test` at a time (154 s); the same packages in one invocation take
    about 20 s.
  - CI's lint job failed whole runs on GitHub 504s while downloading golangci-lint or sqlc release assets.
- **Decision:**
  - **Two local gates.** `make check-fast` is for iterating: vet, custom vet, lint, traceability, the analyzer tests
    and `go test ./...` without `-race` and with the test cache, plus `sqlc-check`/`api-check` when their inputs
    changed since the merge base. `make check` is the pre-push gate. It runs once per task on the final tree and
    checks what it checked before, now fail-fast: vet, custom vet, lint, traceability, analyzer tests, coverage,
    race. AGENTS.md "Gates" says which to run when.
  - **Coverage floor in one invocation.** `scripts/coverage-floor.sh` runs one `go test -count=1 -cover` and reads
    each package's summary line. A package fails when go test reports it failed (tests, build or setup), when it
    has no coverage line, or when its coverage is below the floor. A non-zero exit that no package explains fails
    too. `COVERAGE_PARALLEL` (go test `-p`) bounds how many packages run their tests at once. The default is a
    quarter of the CPUs, at least 1 and at most 8. CI and the release workflow set 1, the concurrency they had
    before. All 28 at once exceeded the 250 ms write-transaction guard on a 4-core hosted runner, so the guard's
    calibration (ADR-0062) is kept. The test binaries are built first with full parallelism.
  - **CI tools from caches.** `bin/sqlc` is cached under the hash of `scripts/install-sqlc.sh`, which holds the
    pinned version and SHA-256 sums. A miss installs it as before (checksum verified), with curl retries.
    golangci-lint is built with `go install …@<pinned version>` through the Go module proxy (modules verified
    against the checksum database), cached per version, OS, architecture and Go toolchain. A step checks the
    binary's version before the action runs it (`install-mode: none`). The release workflow downloads neither tool.
  - **The race run's critical path (T7).** `internal/app` (about an hour of test time under `-race`) alone took about
    9 minutes at `-parallel 4`, with most of a many-core machine idle. Its ~20 serial tests alone take about 260 s,
    since go test runs them one after another before any parallel test. `scripts/race.sh` runs it as its own `go test
    -race` invocation, started first, with `RACE_APP_PARALLEL` (default `min(16, nproc/2)`, at least 4), and on 16 or
    more CPUs splits it into two concurrent processes, the serial tests (`-run`) and the rest (`-skip`, the same
    pattern, so each test runs exactly once; `RACE_APP_SPLIT`), alongside one
    invocation for every other package at `RACE_PARALLEL` (4, unchanged: the small packages starve when everything runs
    at full load). CI and the release workflow set `RACE_APP_PARALLEL=4`, the cap they had.
  - **A node-wide heavy-gate semaphore (T8).** Several agents' full suites on one node slowed each other down and
    tripped the write-transaction guard and timing tests. `scripts/gate-slot.sh` runs `race`, `coverage`, `site-a11y`,
    `e2e`/`e2e-browsers` and `screenshots` while holding one of `SM_GATE_SLOTS` `flock` slots in `SM_GATE_LOCK_DIR`
    (defaults: 2 slots on 16 or more CPUs, otherwise 1; `${XDG_CACHE_HOME:-~/.cache}/spaceman-gates`). `make check`
    takes one slot for coverage and race together. A waiting run prints one line naming the holders. The kernel
    releases a lock when its holder exits, a `kill -9` included. `SM_GATE_SLOTS=0` disables it, and it is off when
    `CI=true`.
- **Alternatives:** `install-mode: goinstall` in the action (it rebuilds golangci-lint from source on every run,
  because the Go module cache key doesn't cover it); caching the golangci-lint release archive (still a GitHub release
  download on a miss); making local `make race` use the test cache (operator question Q1 of the plan, not decided
  here); deriving `RACE_PARALLEL` from the CPU count for every package (T7: the small packages starve their leases and
  write transactions under full load); a semaphore inside SpaceMan, a per-node "heavy job" capacity exposed to runs (a
  possible product follow-up, out of scope).
- **Consequences:**
  - A cached sqlc binary is not re-verified on a hit: the checksum covers the release archive, which a hit doesn't
    download. The cache entry was written by a run of this repository after verification. Actions caches are
    scoped to the branch, so a pull request can't plant one for `main` (THREAT_MODEL T-41).
  - `check-fast` passing is not a pre-push signal: race tests and the strict coverage run happen only in `make check`
    and CI.
  - A heavy gate can wait for minutes behind other agents' runs; that wait costs no CPU. The slots only coordinate runs
    that share `SM_GATE_LOCK_DIR`. Killing only the wrapper releases the slot while its command goes on; stopping
    the process group stops both.

### ADR-0070 — The write-transaction guard measures lock hold time; local race runs use the test cache (security-relevant change)
- **Status:** Accepted, 2026-11-08 (operator approval of the gate wait-time plan, questions Q1 and Q2; subtask T3).
  **Security-relevant change:** it changes what the evidence for Hallpass HP-DB-004 (short write transactions)
  measures. The limits and the rule are unchanged.
- **Context:**
  - `storetest` fails a test when a successful write transaction takes longer than `MaxTxDuration` (100 ms by default,
    `SM_TEST_TX_MAX`; ADR-0062). The duration it got from `store.Writer.TxNamed` started before the first attempt, so
    it also counted the wait for the single writer connection (`MaxOpenConns(1)`), the wait for `BEGIN IMMEDIATE` to
    acquire the database lock, and every `SQLITE_BUSY` retry with its backoff sleep.
  - HP-DB-004 is about how long a transaction *holds* the writer: a long hold blocks every other write. Waiting for the
    writer is a different property, already metered (HP-DB-003: `spaceman_store_writer_wait_seconds`,
    `spaceman_store_writer_queue_depth`, `spaceman_store_tx_busy_retries_total`).
  - Under CPU contention (parallel packages and tests, Argon2 in the auth tests, several gates on one node) the queueing
    made short transactions look long. The guard failed on unchanged code, locally and on hosted runners, in whichever
    transaction happened to queue behind another one.
  - The race run passed `-count=1`, so every local `make race` (and `make check`) reran every package, even after a
    merge that touched no Go code.
- **Decision:**
  - **The observer gets the lock hold time (Q2).** `store.TxObserver` now receives the longest lock hold of the
    transaction's attempts: for each attempt, the time from `BEGIN IMMEDIATE` returning (the lock is held) to the end
    of its commit or rollback, callback and commit included. Taking the longest, not the last, keeps an attempt that
    held the writer for long and then rolled back on `SQLITE_BUSY` from being hidden by a quick retry. It is zero when
    no attempt got the lock. The `storetest` guard compares it with `MaxTxDuration`, and its message now says `held the
    write lock for`. The guard checks failed transactions too (it checked only successful ones), except those that
    ended because their context was canceled or timed out.
  - **What is observed:** only transactions run through `Writer.Tx` and `Writer.TxNamed`. `Writer.Exec`
    (maintenance statements such as checkpoints), migrations and snapshots (`VACUUM INTO`) are not, so the guard does
    not cover them.
  - **Wait and retries stay in metrics.** `spaceman_store_tx_duration_seconds` keeps the end-to-end duration, retries
    and waits included (its help text already said so); writer wait, queue depth and busy retries are unchanged.
  - **Local race runs use go test's cache (Q1).** `make race` passes `-count` only when `RACE_COUNT` is set. CI's race
    job and the release workflow run `make race RACE_COUNT=1`, so every test runs there on every commit; their build
    cache is restored from earlier runs, and a cached pass must not stand in for a run on the commit under test.
- **Alternatives:**
  - **Keep measuring from before the first attempt and raise the limit:** it would hide real long holds in exactly the
    runs where contention is low.
  - **Measure from the start of the successful attempt**, pool wait included: still counts queueing for the in-process
    writer connection, the larger share under `t.Parallel`.
  - **Keep `-count=1` locally:** every rerun of `make check` repays the full race run (about 10 minutes on this
    project's development nodes) for packages that didn't change.
- **Consequences:**
  - A transaction that waits behind another writer for longer than the limit no longer fails a test; one that holds
    the writer for longer still does (`internal/store/storetest` `TestGuardCountsLockHoldNotWait`, `internal/store`
    `TestTxObserverGetsLockHoldTime`, `internal/store/storetest` `TestGuardCountsTheLongestAttempt`,
    `TestGuardChecksFailedTransactions`). Starvation and queueing show up in the writer-wait metrics instead, and in tests
    only when they make something time out.
  - The full non-race suite passes at the 100 ms default (PLAN.md §3). Hold time still includes the commit's fsync
    (`synchronous(FULL)`) and any time the holding goroutine is descheduled, so a heavily loaded machine can still
    trip the guard: on a node where several other suites ran, single-statement transactions held 100–270 ms.
    Those are real holds and the guard reports them.
  - A local cached race pass can come from an earlier run of the same package build and inputs. A flaky test is judged
    with `-count=1` (AGENTS.md "Gates").
  - **Follow-up: re-evaluate ADR-0062's 250 ms on hosted CI and the race relaxations** (500 ms in `make race`, 1 s in
    CI's race job and the release gate). Those values were set against the old measurement, whose failures were
    mostly queueing. Collect the guard's failures (if any) and the hold times of the slowest transactions on hosted CI
    over a couple of weeks of runs with this change; if the coverage job holds 100 ms there and the race jobs hold
    their limits with margin, lower them in a separate ADR. Until then they stay as they are.

### ADR-0071 — The login timing comparison runs on its own; unknown accounts do the same reads (security-relevant change)
- **Status:** Accepted, 2026-11-08 (operator approval of the gate wait-time plan, subtask T5).
  **Security-relevant change:** it changes where the wall-clock evidence for Hallpass HP-AUTH-005 (login response time
  does not reveal whether an account exists) is collected. The guarantee and the deterministic check are unchanged,
  and the product change makes the two paths more alike.
- **Context:**
  - `internal/auth` `TestLoginThrottlingAndEnumeration` checked HP-AUTH-005 two ways: deterministically (exactly one
    Argon2id verification per failed attempt, for an unknown account against a dummy hash with the account's
    parameters), then on the clock (median of 21 interleaved failed logins per kind within 0.75–1.33, best of three
    rounds). The clock half failed under coverage and parallel load on unchanged code, with ratios of 1.34–1.41.
  - Measured per segment (median of 60 interleaved failed logins each; idle node, then while a race run of
    `internal/app`, `internal/store` and `internal/events` loaded the 32-core node to a load average of 57–101):
    - **total ratio known/unknown:** idle 0.84–1.10, loaded 0.68–1.10, in both directions from run to run, under
      plain, `-race` and `-cover` builds alike;
    - **Argon2id verification alone:** the same spread (idle 0.84–1.11, loaded 0.67–1.05). The two hashes have the
      same parameters, so this is CPU scheduling noise on a CPU-bound function;
    - **everything except the verification (database reads, the failure transaction, audit):** known minus unknown
      +0.05 ms idle (about 1 ms per attempt), at most ±2.7 ms under load and `-race`, in both directions;
    - **the reads before the verification:** consistently slower for a known account (+0.02–0.06 ms idle, +1–3 ms
      under `-race` and load), because only that path looked up the local credential.
  - So there is one small real asymmetry, an extra read on the known path, and it is far below the noise the clock
    check sees. The failures came from the verification's own run time under load, which no product change can make
    equal.
- **Decision:**
  - **Same reads on both paths.** `Login` looks up the local credential for unknown accounts too, by an ID no user
    has. After the change the pre-verification delta is 0.02–0.25 ms in plain, `-race` and `-cover` builds (from
    0.06–3.2 ms). The failure transaction already did the same writes on both paths (one throttle upsert per key, one
    audit row).
  - **The deterministic check stays in every suite** (`TestLoginThrottlingAndEnumeration`: `make test`, `race`,
    `coverage`, `check-fast`, `check`, CI and the release gate).
  - **The wall-clock comparison moves to `TestLoginTimingWallClock`**, with the same rounds and band. It runs only with
    `SPACEMAN_LOGIN_TIMING=1` (`make test-login-timing`), and skips under `-race` and `-cover`. CI runs it as its own
    job (`login-timing`) on every pull request and push, alone on its runner, and the release workflow runs the same
    job as a gate before `binaries` and `images`.
  - **The equal reads are tested deterministically.** `TestLoginReadsSameForUnknownAccounts` records every query a
    failed login runs through the store's readers (`storetest.WithReadObserver`) for a known and an unknown account,
    and requires the same sequence, including the `GetLocalCredential` read. With that read removed from the unknown
    path again, it fails.
- **Alternatives:**
  - **Widen the band:** a band wide enough for a loaded node (0.6–1.6) no longer tells a real asymmetry from noise
    either. What matters, a missing or cheaper dummy hash (orders of magnitude), the deterministic check already
    catches.
  - **More samples or more rounds:** the skew comes from load that persists for seconds, so it moves every sample of a
    round. It would also slow the suite further.
  - **Skip it only under `-race` and `-cover`:** the plain `make test` and `check-fast` runs share the CPU with every
    other package too.
- **Consequences:**
  - A local `make check` no longer measures login timing. CI's `login-timing` job does, on every change. Locally,
    `make test-login-timing` on a quiet machine.
  - A change that makes one path much faster or slower still fails the deterministic check, unless it keeps exactly one
    same-parameter verification. A small asymmetry outside the verification (like the extra read fixed here) shows
    only in the clock job, and only once it exceeds the band. An extra or missing read before the verification also
    fails `TestLoginReadsSameForUnknownAccounts`.
  - **Residual risk (accepted, not changed):**
    - **Argon2id verification variance.** The verification is CPU-bound and its run time varies with scheduling and
      load (0.67–1.11 between two same-parameter hashes in the measurements above). That noise hides small
      differences rather than revealing the account, but it also means the clock check only bounds the asymmetry, it
      does not prove it is zero.
    - **Network and remote timing are not measured.** The ±25% band is checked in-process on a quiet runner. Timing as
      seen over the network, through proxies, or on a loaded production host is not tested; the bound only applies on
      a quiet machine.
    - **Accounts hashed with older parameters.** The dummy hash uses `CurrentParams`. An account whose stored hash
      still has an older parameter set verifies at that set's cost until its next successful login rehashes it, so
      for such an account a failed login can take measurably longer or shorter than one for an unknown account. This
      lasts only while older-parameter hashes remain, and the deterministic check covers accounts on the current set.

### ADR-0072 — `github-pull-request` version 2: ignore your own agent's pushes and edits; built-ins gain versions on existing installations
- **Status:** Accepted, 2026-11-08 (operator approval of the gate-wait plan's T12 and T13).
- **Context:** In one space, about 95 of roughly 130 pull request inbox items in 2.5 days were `synchronize` or
  `edited` events that the space's own agent account caused (its pushes and pull request body edits). Each woke
  control for a minute or more. The six ADR-0061 templates take `exclude_author`, but `github-pull-request` version 1
  has no author or action filter, and it puts the pull request title into the item title. Changing a published
  built-in needs a new version, and `SeedBuiltins` only created missing templates, so existing installations would
  never have received one (ADR-0061 consequences).
- **Decision:**
  - **Version 2 of `github-pull-request`.** Same subject (`evt.github.<account>.webhook.pull_request.*`) and
    correlation. It takes the ADR-0061 scope variables: `repository` (now optional), `owner` and `exclude_author`
    (the webhook sender's login, normally the space's agent account). A new flag `ignore_updates` (only `true`) adds
    `data.github.action ne synchronize` and `ne edited`, so new commits and title, body or base edits are dropped and
    every other action (opened, closed, reopened, ready_for_review, review_requested, …) still passes. Bots are kept
    (a bot's pull request can need review). The item title holds only the repository, number, action and login; the
    pull request title moves to the body as inline code (T-03).
  - **Built-ins gain later versions.** `builtinLaterVersions` lists a built-in's versions after version 1. At every
    start `SeedBuiltins` publishes the missing ones in order, as `system`, and only on top of the built-in's own
    newest version: if an administrator has made their own version or draft of the built-in, that version is never
    replaced or renumbered, and seeding stops there with a warning. Version 1 and subscriptions pinned to it are not
    touched. Subscriptions that follow the latest version move to the new version when it binds with their variables
    and keeps their subject, exactly as on Publish (SM-EVT-006). A version 1 subscription's variables bind to
    version 2 unchanged, so followers move. They then get the new title format, but their filters stay as before
    until they set the new variables. The publication is audited as `event_template.published` by the system actor.
- **Consequences:**
  - Existing installations get version 2 at their next start. Pinned subscriptions must be switched by hand
    (`spaceman event subscription update --template-version …`, or the API). The web UI cannot change a subscription's
    template version or variables yet: switching there means a new subscription, which needs a new Pulse grant.
  - On releases without version 2 (v0.1.0-rc.9 and later), a custom template with exactly version 2's definition
    gives the same result (site guide: Events, "Ignoring your own agent's pull requests").
  - Not solved here: several events on one pull request still wake control once each (coalescing by correlation is
    a separate, optional follow-up).

### ADR-0073 — Attachments of any size reach a run as a file in its own folder; the inline read never truncates silently
- **Status:** Accepted, 2026-11-08 (operator request: screenshots over 512 KiB could not reach agents).
  **Security-relevant change:** the node agent now writes attachment content (untrusted, named by its uploader) into
  the run's own instance folder.
- **Context:**
  - `spaceman_attachment_get` returned at most 512 KiB, with only a `truncated` flag that was easy to miss, as ~700k
    characters of base64. For an operator's 668 KB screenshot, the agent got 524 288 bytes it could not decode. The
    result was also too large for Claude Code's tool output, so it spilled to a temporary file anyway.
  - One node-protocol envelope is at most 4 MiB (`proto.MaxMessage`), and uploads go up to 25 MiB by default, so a
    whole attachment can't travel in one tool result.
  - The run's file tools read its own space folder without a dialog (ADR-0068), and Claude Code's Read tool shows
    images (scaling large ones down itself).
- **Decision:**
  - **Ranged inline reads.** `spaceman_attachment_get` takes optional `offset` and `length` (at most 1 MiB,
    `proto.AttachmentChunkBytes`; 512 KiB when no range is given). Every result says `truncated`, `offset`,
    `returned_bytes`, `next_offset` when more follows, and a `note` on how to get the rest. `offset` given (0
    included) makes a ranged read. `next_offset` is present exactly when `truncated` is; after a ranged or partial
    read it is past `offset`, so following it always makes progress (review S2: a short `length` on multibyte text
    once returned 0 bytes; now a chunk that can't be cut on a character boundary goes whole, as base64).
    - A **binary** file over 512 KiB asked for without a range is **not returned inline**: just its record,
      `truncated: true`, and a note pointing to `spaceman_attachment_save`.
    - **Text** comes back in chunks cut on a character boundary.
  - **`spaceman_attachment_save`, served by the node.** The MCP bridge offers it when the run's catalog has
    `spaceman_attachment_get` (so the same run kinds may use it), and the node agent does the work:
    - It copies the attachment through the existing `tool.call` path in ranged reads, so central authenticates and
      authorizes every chunk exactly as for the inline tool. There is no protocol change and no new central route.
    - The file is `<space folder>/instances/<instance>/attachments/<run>/<attachment>/<name>`, mode 0600 in 0700
      folders. It is inside the space folder the run may read (ADR-0068) and outside every protected config path.
    - The size and SHA-256 are checked against central's record before the temporary file is renamed into place. A
      chunk from a different record, a wrong range or a mismatch fails the call and leaves no file.
    - The result is the real path, size, SHA-256, MIME type and name.
    - One save is at most 50 MiB (above central's default 25 MiB upload limit), so a run can't fill the node's disk
      through it. A larger attachment is refused before anything is written; ranged reads still work.
  - **Untrusted names.**
    - The uploader's name is reduced to one plain element: the last element (either separator), `[A-Za-z0-9._-]`
      (anything else becomes `_`), no leading dot, at most 100 bytes keeping a short extension, and `attachment` when
      nothing is left.
    - The attachment ID must be a valid `att_` ID, and the run and instance IDs valid too.
    - **No run-writable folder is resolved by path** (review M1). The run can rename folders and plant links
      anywhere in its space folder: its file tools may write there (ADR-0068) and its shell runs as the node's user.
      The first version checked the attachments folder with `Lstat` and then opened it by path, and a run that swapped
      the folder for a link between the two won the race (the save wrote into the link's target; the prune emptied
      it). Now the node opens an `os.Root` on its own `<data_dir>/spaces` folder (outside every space folder; a run's
      file tools are not given it) and walks `<space>`, `instances`, `<instance>` and `attachments` one level at a
      time, each relative to the open handle above. Each level is opened first and checked after: `Lstat` through the
      parent handle must show a real folder and `os.SameFile` must match the opened handle, so a link in place before,
      between or after the two steps is refused, never followed (an `os.Root` alone would follow a link that stays
      inside it, for example to the instance's own `claude/` folder). Every later mkdir, write, rename, listing and
      removal is relative to the final handle. Residual: a run that can rename folders can move the opened folder
      elsewhere while a save is writing; the save then writes the attachment's bytes, under its sanitized name, into
      that moved folder, and a prune removes only entries of that folder. Both are things such a run could already do
      itself.
  - **Cleanup.** The run's folder is removed when the run ends, including runs the node finds lost or never started
    when it reconciles with its runner after a restart (review S1). A save re-checks that the run is still running
    before and after renaming its file into place, and removes it when the run has ended meanwhile (S4), and one save
    is bounded as a whole by `saveTimeout`, 10 minutes (S3). A launch of the instance removes the folders of runs
    that ended or that the journal no longer knows. The folder is per run because nothing stops two runs of one
    instance.
  - The built-in orientation tells every run to use `spaceman_attachment_save` for images and anything over 512 KiB.
    The same tools serve every attachment reference: inbox items, tasks, human actions and messages list attachment
    records and `spaceman://attachment/<id>` links.
- **Alternatives:**
  - **Chunked reads only** (the model reassembles base64 itself): the data passes through the model's context
    and is easily corrupted. An earlier agent dropped about 2 KB retyping one by hand.
  - **A server-side downscaled preview** (≤ 2000 px, re-encoded): needs an image library or a hand-written resampler
    in central, decodes untrusted images in central, and puts the preview into the context on every call. Claude Code's
    Read already scales images down, and Codex views local images. It can be added to the inline result later if a
    harness can't read files.
  - **A signed short-lived URL** the agent fetches: a new authenticated HTTP surface and a bearer credential in the
    model's context, and the agent's shell would need network access to central.
  - **Central writes the file:** central has no access to the node's filesystem. The node already holds the run's
    authenticated connection.
- **Consequences:**
  - Attachments of any size (up to the upload limit) reach a run whole, checked, and readable with its own tools
    without a dialog. Verified live with Claude Code 2.1.319: Read shows a saved 2.8 MB PNG as an image in
    `acceptEdits` and `default` modes, while the same file outside the space folder is denied by SpaceMan's hook.
  - A saved file takes node disk space until the run ends, at most the attachment sizes the run saved.
  - A node agent from before this release doesn't offer the save tool. A newer node with an older central answers
    "upgrade central".
  - Other runs of the space can read a saved file through the space folder (ADR-0068), as they could fetch the
    attachment themselves. They can't write it: other instances' folders are denied for `Edit`.

### ADR-0074 — Concurrency limits count live runs only; a stale-run sweeper ends runs whose process is gone
- **Status:** Accepted, 2026-11-09 (operator request after a node agent restart left its runs holding slots).
  **Security-relevant change:** a disconnected node's runs no longer stay `unknown` indefinitely: once the space's
  lease, the disconnect grace and a 15-minute margin have passed, central records them as `lost` (SM-NODE-005 refined
  below). A run swept wrongly is killed by the fence at its next report, so the safeguards below are part of the decision.
- **Context:**
  - The space concurrency limit and the node capacity counted runs with `desired = 'running' OR observed IN (pending,
    starting, running, stopping, unknown)`. A run that ends on its own (exits, fails, or is reported lost after a node
    agent restart) keeps `desired = 'running'`, so it held its slot until an operator stopped it, while
    `spaceman_runs_list`, which hides exited, failed and lost runs, showed nothing to explain the refusal.
  - A run central still records as live but which its node never had (a launch lost with the agent's state) is not in
    the node's journal, so no report ever ends it: it stays `running` or `unknown` and holds a slot forever.
  - The node agent reports every run in its journal in its hello, and central applies those reports before anything
    else (SM-NODE-009). Without central, the node's lease guard stops a space's runs once its lease and the disconnect
    grace pass (SM-NODE-007); `SweepGatewayLeases` already relies on that to revoke gateway tokens.
- **Decision:**
  - **Live runs.** A run is live while its observed state is pending, starting, running, stopping or unknown
    (`fleet.Live`); desired is not part of it. `spaceman_runs_list`, the space limit and the node capacity all use that
    one definition (`ListLiveWorkRunsInSpace`, `ListLiveWorkRunsOnNode`; a store test checks they agree for every
    observed and desired state). Which kinds count is unchanged: worker and diagnostic runs; control, KB support and
    dreamer runs have their own pools (SM-LIMIT-002).
  - **Refusals name what they count.** The space refusal lists the counted runs (ID, kind, role, observed state); the
    node refusal counts other spaces' runs without naming them and lists this space's. Both say which runs don't count.
  - **Stale-run sweeper** (`fleet/stale.go`, on the deadline timer, woken on node connect and disconnect). Its rule:
    it must never end a run whose process exists or might still start; when in doubt, it doesn't sweep. It ends a
    run as `lost` on one of two signals:
    - **`stale_not_on_node`**: the node is connected, and its hello reports every run in its journal. The
      `fleet.node_connect` transaction reads the node's live runs (with their versions), and the session records the
      run IDs the hello contained. A run is swept only if it was live at connect, the hello didn't contain it, its
      version hasn't changed since connect, and `staleRunGrace` (2 minutes) has passed since the connection, measured
      on the clock's monotonic reading (a wall-clock step doesn't move it). No stored timestamp is compared. If any
      hello report failed to apply, the sweeper leaves that connection's runs alone entirely. A run created after the
      connection began is never a candidate.
    - **`stale_lease_lost`**: the node isn't connected, and the space's lease on that node expired more than the
      disconnect grace plus `staleLeaseMargin` (15 minutes) ago. The grace is the larger of the one the lease was last
      granted with (stored on the lease, migration 0046) and the current one, so lowering the grace in the config
      doesn't shorten the wait. The node's lease guard has stopped the process by then.
    - Never candidates: pending runs; runs with an unacknowledged launch command (`pending` or `sent`: the dispatch
      loop may still deliver it); runs no node has ever reported (`observed_at` is NULL: a pending launch turns
      `unknown` on disconnect without one); runs whose space now belongs to another node (relocation ends them).
  - **Clock assumption.** The node's lease guard compares central's lease expiry with the node's own clock, with no
    skew correction. `stale_lease_lost` therefore assumes the clocks of central and the nodes are synchronised (NTP).
    The 15-minute margin is far beyond any plausible skew of a synchronised clock; a node whose clock runs more than
    that behind central's could still be running a process when central records it lost.
  - **End transaction.** Each run ends in its own transaction, which re-checks the signal: the run's version is
    unchanged since it was chosen, it is still live past its launch and has no unacknowledged launch, and for
    `stale_not_on_node` the node's connection is still the one whose hello didn't contain it, for `stale_lease_lost`
    the node is still not connected. A report, a reconnect or a queued launch at the same time wins. The run then ends
    through the node-report path: credentials and gateway tokens revoked, `run_ended` published, `OnRunEnded` called.
    The same transaction cancels every undelivered or unacknowledged command of the run and sets its desired state to
    `stopped`, so `sendReconcile` stops listing it to the node, and archive and relocation aren't blocked by it. The
    sweep is idempotent (an ended run is no longer a candidate). A central restart mid-sweep finds the remaining runs
    again on the new connections, after a new grace.
  - **Safety, and what a false positive costs.** The sweeper itself never sends a stop or a kill. But once a run is
    swept, any later report of it is fenced, and a report that it is still running is answered with the fence's
    `stop kill` (SM-NODE-006). So a run swept wrongly is killed at the node's next report of it. The safeguards
    against that are the rules above: only runs that were live at connect and missing from a hello that applied in
    full, never a pending launch or one never reported, a monotonic grace, a 15-minute margin on a lost lease with the
    largest grace, and the re-check in the end transaction.
  - **Visibility.** The run shows end reason `stale_not_on_node` or `stale_lease_lost` and an explanation in its error.
    An audit record `run.reconciled_stale` names the reason and the previous state, and
    `spaceman_runs_reconciled_stale_total{reason}` counts sweeps.
- **Consequences:**
  - **One-time effect on upgrade:** runs that had already ended but kept `desired = 'running'` stop counting at once.
    Spaces that were refused may admit new workers right after the upgrade.
  - **One-time sweep on the first deploy.** Every historic run left `running` or `unknown` that meets the rules (after
    the nodes reconnect and the grace passes, or 15 minutes past a lost lease) is ended in the first sweeps. Each one
    publishes `run_ended`, has its credentials and gateway tokens revoked and its desired state set to `stopped`, and
    goes through `OnRunEnded`. For a worker run, that tells the space's control the run ended and names its task;
    control may then re-queue or relaunch that task, so old tasks can come back to life. Operators should look at
    these notices (audit action `run.reconciled_stale`) and close tasks that are no longer wanted. A swept control run
    isn't restarted by the control supervisor (its desired state is `stopped`), so a space whose control is swept
    needs control launched again.
  - **Fencing trade-off.** Once swept, a run stays exactly as the sweeper ended it: any later report for it is fenced.
    A node that reconnects after `stale_lease_lost` and reports the run's real exit (its exit code) doesn't change the
    record.
  - SM-NODE-005 is refined, not withdrawn: execution on a disconnected node is `unknown` while it may still be running.
    It is never recorded as `failed`, and it becomes `lost` only once the lease guard must have stopped it.
  - Migration 0046 adds `execution_leases.grace_us`. No node-protocol change.
  - ~~Follow-up: `ListActiveRunsBySpace` and `ListActiveRunsByNode` still count ended runs with `desired = 'running'` as
    active.~~ Resolved by the amendment below.
- **Amendment (2026-11-09, review follow-ups of #28):**
  - **Active runs are live runs.** `ListActiveRunsBySpace` and `ListActiveRunsByNode` use the live predicate
    (`fleet.Live`), not `desired`. A run that ended on its own no longer blocks archiving the space, a relocation without
    `stop_runs` or a relocation's drain, and isn't counted in "N active run(s)". Pausing with interrupt or emergency no
    longer queues a stop for such a run. `sendReconcile` needs nothing else: the node reports every journal run whether
    or not it is listed and only acts on runs listed as `stopped` or with another generation, and an ended run with
    `desired = 'running'` was listed as `running`, which the node treats like an unlisted run.
  - **Only active leases count for `stale_lease_lost`.** `RevokeLease` stores the revoke time as the expiry, but the
    node keeps the expiry it was last given, up to `lease_ttl` later. A revoked lease is therefore not a basis for the
    sweep: those runs wait for the node to report them. `nodes.lease_ttl` is also bounded at 10 minutes
    (`config.MaxLeaseTTL`), well inside the 15-minute margin.
  - **A replaced connection's end records nothing.** Before, when a node reconnected while its old connection was
    still open, the old connection's end still recorded a disconnect and marked the node's runs `unknown`, over the new
    connection's state. Each connection now gets a sequence number and claims the node in its `fleet.node_connect`
    transaction; the disconnect transaction records the disconnect only if its connection is still the node's newest.
    Write transactions are serialized, so a disconnect and a reconnect can't interleave.
  - No migration and no protocol change.

### ADR-0075 — Run terminal sizes: per run, held by the node through an attach that never types
- **Status:** Accepted, 2026-11-09 (operator request for v0.1.1: "configurable terminal size from web"; design agreed
  with control before building).
- **Context:**
  - herdr-core's `terminal.run` takes no size. A program starts at the core's `headless` size, which the node sets
    from `runner.terminal_size` (default 160×48, S-6). The web view scaled the font to fit a run's columns, but the
    size itself could not change.
  - An interactive attach resizes a program only while it is open. Once it closes, the program goes back to the
    headless size. With two interactive attaches open, the newer one (or its later resize) sets the size, and closing
    it brings the other one's size back. `events.subscribe` reports each change as `size {cols, rows}`. These
    behaviours were verified on hc 0.1.17 (docs/contracts/herdr-core.md S-7, `TestRealTerminalSizeHold`).
  - The recorder attached as an observer and wrote a `size` record only when it re-attached. A live resize would
    therefore have replayed at the wrong size.
- **Decision:**
  - **The size belongs to the run, not to a viewer.** An operator with `terminal:takeover` in the run's space sets it,
    the same permission as takeover, because it changes what the agent's program sees. While the terminal is taken
    over, only the holder may change it. With two operators, the last change wins. Every viewer sees the run at its
    real size, and the browser scales the font to the viewer's window or scrolls horizontally, as before. Viewers
    without the permission get no control. Operators see it disabled, with the reason.
  - **Central stores it.** Migration 0047 adds `runs.terminal_cols` and `terminal_rows`. Both are NULL (the node's
    default) or both set, with 40–400 columns and 10–200 rows, the bounds of `runner.terminal_size`.
    `PUT /spaces/{space}/runs/{run}/terminal-size` sets the size and `DELETE` clears it. Both are audited as
    `run.terminal_resized`, with the previous size. The run's `terminal_size` shows it, and `terminal_sizing` says
    whether the run's node can hold a size now. The admin MCP excludes both operations (a person's at the keyboard,
    like takeover).
  - **Node protocol minor 10, additive.** Central sends `terminal.sizes`, the complete set of the node's sized runs
    (space, run, generation, cols, rows), after the node connects and on every change, the same pattern as
    `takeover.state`. For each listed run whose space lease and generation are valid, the node opens an interactive
    attach on a connection of its own (`runner.Takeover`, at the current size) and resizes it. The hold **never
    writes**: no input reaches the program through it. The node closes a hold when the run leaves the set, when the
    run ends, when the space's lease is revoked or expires (fencing), and at shutdown. A run that is not live yet is
    held once it is. A failed attach is retried while the size is still wanted. An open takeover attach is resized
    along with the hold, and back to the node's default when the hold closes. A node below minor 10 is never sent the
    set, and resizing one of its runs fails with **409**.
  - **Recordings.** The recorder writes a `size` record for every change. The node notes the size itself before it
    resizes or releases a hold, so the program's redraw for the new size follows the record. The runner's `size`
    events note changes the node did not make (a takeover attach, a hold that dropped). A pending size note is written
    before the next output chunk. Replay already redraws at `size` records.
  - **Default for new runs:** the space limits `terminal_cols` and `terminal_rows` (installation defaults in
    `limits:`, 0 = the node's default). A size applies only when both are set. Each must be 0 or within the bounds.
    Launch resolution copies them to the new run, and the node holds it from the start. A node connected below minor
    10 cannot hold it, so its new runs keep no size (they would show one they don't have). **Changing either key needs
    `terminal:takeover`** in the space, like resizing a live run: `set-space-limits` otherwise needs only
    `space_config:write`, which operators hold, and that would let them set every new run's held size. A change that
    leaves both keys as they are needs only `space_config:write`, the same way retention periods need
    `retention:write` (review of PR #33, S1).
  - **Node holds are race-free.** `reconcileHolds` plans under the agent's lock and opens holds after unlocking, so
    `startHold` checks again under the lock before it records a hold: the size is still wanted exactly as planned,
    the space's lease is valid and the agent has not stopped. Otherwise it opens nothing. Central serializes its
    `terminal.sizes` pushes per node session (read and send under one lock), so an older set is never sent after a
    newer one.
  - **Web:** the live terminal has a size menu: Fit to window (the terminal area at the regular font size, refitted
    after the window changes while it stays applied), 80×24, 120×40, 160×50, 200×60, Custom… and Node default. The
    browser remembers the last choice and shows it selected, but **never applies it on its own**, so opening a run's
    page never resizes it for everyone else.
    - Fit refits only when the browser's window or visual viewport changes size, never when the terminal box does:
      the box follows the run's size, so refitting on it undid other people's resizes. Once the run's size differs
      from the last size this browser sent (someone else resized or cleared it), the browser stops fitting and shows
      the run's size as the selected choice.
    - The fit measures the regular cell the way ghostty-web 0.4.0's renderer does (`measureText("M")` at the
      regular font size), not by scaling the cell shown now, so the same window always fits to the same size.
    - When the viewer can't resize, the control stays focusable (`aria-disabled`) and its reason is its accessible
      description. The choices are `menuitemradio` items with `aria-checked`.
- **Alternatives:**
  - Sizing tied to a takeover. This needs no migration, but the size is lost on release and resizing needs a takeover
    that pauses automated input. Rejected by control.
  - Per-viewer sizing. A program has one size, so viewers would fight over it.
  - Restarting the core with a new `headless` size. That changes every new program on the node and never a running one.
- **Consequences:**
  - One extra herdr-core connection per sized run on the node.
  - This is not a security relaxation: the permission is takeover's, the hold types nothing, and the size is bounded
    and audited.
  - CLI: `spaceman run terminal-size set|clear` resizes a live run. `spaceman limits set --set terminal_cols=… --set
    terminal_rows=…` takes the new limits as they are (its keys are generic).
  - Terraform: the provider's `spaceman_space_limits` resource declares one attribute per known limit, so it cannot
    set these two yet. Its data source shows them in `values`. Adding the attributes is a follow-up in the provider
    (PLAN.md).

### ADR-0076 — The web terminal loads stored output in slices and works around ghostty-web 0.4.0's per-row viewport parse
- **Status:** Accepted, 2026-11-12 (operator report on v0.1.1: opening Terminals or Spaces on a fresh cache froze the
  whole tab for about a minute, and for minutes on a phone).
- **Context:**
  - A cold-cache Chromium trace with CPU throttling (2026-11-12) found the cause. It was neither the WebAssembly loading
    nor the chunk-reload logic: it was the stored output a terminal applies when it opens.
  - A live terminal's stream starts with the run's whole recording (catch-up), which is tens of megabytes for a long
    Claude Code session. Each record was applied in the task that delivered it, with a React render per record. A
    replay applied the whole recording in one task (the former `renderTo`).
  - ghostty-web 0.4.0's `GhosttyTerminal.getLine(y)` copies and parses the whole viewport to return one row, and its
    renderer calls it for every row it draws. A full redraw therefore costs rows × rows × columns cell parses (2.9
    million at 200×120). Output that scrolls changes every row, so each frame during catch-up redrew the whole
    screen. The renderer also calls `fillText` for every blank cell.
  - For a 22 MB recording at 4× throttling, the main thread was saturated for 33 s: 123 long tasks, the longest
    933 ms. With 5 MB (`e2e/terminal-load.spec.ts`), the longest task was 917 ms live and 1.5 s in a replay (5.0 s at
    6×), and a key press waited up to 2.8 s.
  - #27 (ADR-0052's 2026-11-09 amendment) gave every terminal its own WebAssembly instance through `Ghostty.load()`. That
    fetched and compiled the module again from a 560 kB `data:` URL for every terminal. Compiling is asynchronous, so
    this was not the freeze, but the work was repeated for nothing.
- **Decision** (`web/src/features/terminal/`):
  - **Stored output is applied in slices** (`records.ts` `feed`). Records are queued and applied in slices of about
    12 ms, then the page gets the main thread back. Consecutive output records go to the terminal in one write of at
    most 32 kB. The badges update twice a second, and "Loading output…" shows until the terminal has caught up. A
    replay seek works the same way, and playback waits for it to finish. A replay reads its recording in pages of
    1000 records, so each response parses in a short task.
  - **No drawing while catching up** (`ghostty.ts` `holdDrawing`). The terminal's renderer is paused from the
    stream's first record, or while a backlog remains, and draws once in full when the backlog is gone. `fitFont`
    holds drawing the same way while it steps through font sizes.
  - **Two patches to ghostty-web 0.4.0's prototypes**, applied once:
    - `cacheViewport` keeps the parsed viewport until the terminal changes (`write`, `resize`, `free`) or the current
      task ends, so a redraw parses the viewport once.
    - `skipBlankCells` skips the text call for a blank cell unless it is underlined, struck through or part of a
      hovered link. A blank cell has no ink, and its background is drawn separately.

    Both patches are checked against the real module (`viewport.test.ts`) or the 0.4.0 renderer's contract
    (`ghostty.test.ts`). They must be re-checked when ghostty-web is upgraded. Upstream ghostty-web has the defects;
    these are our side's smallest fixes (upstream report pending: drafted, not filed; the operator decides).

    The patches are applied only when the installed ghostty-web is exactly 0.4.0 (`patchGhostty`, against the
    version Vite reads from the package at build time). On any other version they are skipped with a console warning:
    drawing is slower again, never wrong. `version.test.ts` fails while the package is not 0.4.0, so an upgrade
    re-verifies them first.
  - **The module is compiled once per page** from the package's `ghostty-vt.wasm`, served as a hashed, immutable asset.
    Each terminal still instantiates it. The module defines its own memory and imports nothing but a log function, so
    every terminal keeps a fresh, zeroed memory of its own and no shared state, as #27 requires.
    `check-dist` fails a build without the asset.
  - **ghostty-web's inlined copy of the module is stripped from the build.** Its `Ghostty.load()` carries the module
    as a base64 `data:application/wasm` URL (564 kB of the terminal's chunk), which the page never uses. A Vite
    transform (`vite.config.ts` `stripGhosttyInlineWasm`) replaces it with a URL that fails to load, so a stray
    `Ghostty.load()` falls back to the package's `./ghostty-vt.wasm`. The `RunWorkbench` chunk goes from 687 kB
    (204 kB gzipped) to 122 kB (37 kB). The transform fails the build when it finds nothing to strip, and `check-dist`
    fails it when any `data:application/wasm` is left in `dist`.
- **Consequences:**
  - At 4× CPU throttling on a cold cache, with 5 MB of stored output, the longest task is now 96–129 ms while a live
    terminal loads and 111–131 ms in a replay. At 6× they are 174 ms and 228 ms. A key press is handled within
    0.6 s at 4× (2.8 s before). With 22 MB, the 33 s of blocked main thread became tasks of at most about 0.26 s
    (the longest is the page's own first render), and the page answers input while the output loads.
  - A terminal shows its previous picture and "Loading output…" until it has caught up, instead of drawing each
    intermediate screen.
  - Opening a page still costs React's render: Spaces takes 140–250 ms at 4× (the same before and after), and the
    Terminals page's first render about 420 ms. The change feed's refetch burst on load is one task. Neither is
    terminal work. They are the next things to measure if pages still feel slow on a phone.
  - ADR-0020's `connect-src data:` is no longer needed and is removed (see its update).
- *Update 2026-11-13, stale glyphs (operator report on v0.1.1: a watched 160×48 control run showed older lines of the
  same session through its new lines, character by character):*
  - **Root cause: ghostty-web 0.4.0's WebAssembly module, not the renderer.** The run's recording was complete
    (11,407 records, no sequence gaps, no size changes). Fed to the 0.4.0 module alone, with no canvas, it produced
    the screenshot's rows in the terminal's own cells. Replayed record by record against xterm.js, it first diverged
    when rows scrolled in at the bottom: they still held cells of output from hundreds of lines earlier. Upstream
    ghostty-web #180 (`bec9e16`, first released in 0.4.0-next.18): `Screen.cursorDownScroll()` cleared a newly grown
    row only when the cursor's style had a background colour. After `ESC[0m`, the row kept whatever its reused page
    memory held.
  - Claude Code shows this because of how it writes a line: it places each word with a column move (`CSI n G`) and
    skips the cells between words, which it expects a fresh row to have blank. Those cells showed the stale text, as
    in `losparseeline 14xxxx…`. Truecolour output that ends each line in a reset triggers it after a few hundred
    lines (`scrolled.test.ts`). Not the cause: glyph overhang between rows, the dirty-row redraw set, the scaled
    fixed size, the device pixel ratio, or catch-up slicing. A simulated renderer drew exactly the module's cells.
  - **Decision:** the terminal keeps ghostty-web 0.4.0's JavaScript and loads the WebAssembly module of
    `ghostty-web@0.4.0-next.21.g640db46` (2026-10-28), which contains the fix. It comes in as the pinned package
    alias `ghostty-web-vt` (`web/package.json`; the lockfile holds its integrity hash), and no binary is committed.
    The module exports everything 0.4.0's does, plus two hyperlink functions, imports only the same log function,
    and takes the same configuration layout. Replayed under 0.4.0's JavaScript, the whole control recording matches
    xterm.js after every record.

    The JavaScript is not upgraded. Its about 2,000 changed lines include a new input handler (beforeinput, mouse
    tracking), which would need a fresh review of watch-only (SM-UI-004), and the prototype patches above are
    verified only on 0.4.0. `version.test.ts` pins both versions, so changing either fails until the pairing is
    re-verified. `vtmodule.test.ts` checks the pairing automatically: the module exports every function 0.4.0's
    JavaScript calls, imports only the log function, and reads the configuration in 0.4.0's layout (colours,
    palette, scrollback limit). The pairing is temporary. It goes away when the terminal upgrades to an upstream
    ghostty-web release that contains the fix: a follow-up task, with a fresh SM-UI-004 review of the new input
    handler and the prototype patches re-verified.
  - **Tests:** `scrolled.test.ts` runs the stub's pattern against the shipped module (blank rows) and against 0.4.0's
    module (the defect). `e2e/terminal-stale.spec.ts` drives the stub harness's new `/sparse 1000` line live and in
    replay, at DPR 1 and 2, and at the node's 160×48 both unscaled and scaled down to fit. It checks that every row's
    cells read what the program wrote, and that the canvas has only background after each row's text. Both checks
    fail on 0.4.0's module. The browser test probe now reads an empty cell as a space: ghostty-web's
    `translateToString` drops it, which shifted the rest of the row.
  - **Side finding, not changed:** ghostty's `scrollbackLimit` is in bytes, but ghostty-web passes the `scrollback`
    option through as-is, so `scrollback: 10_000` keeps only about 290 lines at 160 columns.

### ADR-0077 — Node-managed Claude Code versions (security-relevant change)
- **Status:** Accepted, 2026-11-13 (the operator chose the managed design and authorized shipping it in v0.1.2;
  proposed 2026-11-13). The operator decided on 2026-11-12: always use the latest version, unless the harness profile
  pins one; then use that version, downloading it if it is missing or reverting to it. Implemented in PRs #41–#44
  (PLAN.md, CC). This ADR is security-relevant because **the node downloads and executes
  vendor binaries** that no operator installed by hand.
- **Context (code, at v0.1.1):**
  - SpaceMan doesn't manage Claude Code. The node runs whatever `claude` the run's `PATH` resolves to
    (`harness.claude_binary`, default `"claude"`). `Detect` resolves it on the agent's `PATH` when the node connects,
    but the launch shim resolves it again on the herdr-core core's `PATH`, so the reported version and the binary a run
    executes can differ.
  - The reported version is refreshed only on a reconnect more than 5 minutes after the last detection. The run
    manifest's `dependencies.claude-code` copies that value when central creates the run, not what the node launched.
  - SpaceMan never sets `DISABLE_AUTOUPDATER`, so every interactive Claude run updates itself into the node agent's
    `HOME` (about 340 MB per version). The spike below shows this happens even when the binary was launched from
    elsewhere.
  - No profile or node setting can choose a version.
  - Release metadata (contract: `docs/contracts/harnesses.md`, Distribution): channel pointers `latest` and `stable`,
    a per-version `manifest.json` with each platform's SHA-256 `checksum` and `size`, and two signature sidecars.
    Older versions stay downloadable.
- **Decision:**
  1. **Resolution precedence: the profile pin, then the node channel, then `latest`.**
     - A new typed profile field `harness_version` (for `claude-code`; keyed by harness kind so Codex can follow)
       takes `""` (follow the node), `latest`, `stable` or an exact `X.Y.Z` checked with a strict regexp. API, web
       and Terraform validate it. Central passes the pin to the node as `LaunchPayload.harness_version`.
     - The node's `harness.claude.channel` (default `latest`) accepts the same values.
     - The node resolves, because it owns the network, the cache and the platform. An exact version is used as is
       and downloaded first if it is missing (the run waits in `starting`), so a pin "reverts" by being launched
       again. A channel is resolved with `GET {base_url}/{channel}`, validated as strict semver and memoised for
       `refresh` (default 15 min). A background loop re-resolves and prefetches. If a channel's target isn't cached
       yet or the pointer can't be fetched, the run uses the newest verified cached version and reports both the
       requested and the actual version. With nothing cached, the launch waits up to `download_timeout`.
  2. **A fixed HTTPS host; `base_url` is operator-only.** The default is
     `https://downloads.claude.ai/claude-code-releases`, the same base compiled into Claude Code. `base_url` is node
     configuration only (so an operator can mirror it), must be `https`, and is never settable from a profile, space,
     API or MCP. Request paths are built only from a validated version and a platform from `runtime.GOOS/GOARCH`
     (plus a musl check on Linux), never from response text.
  3. **Integrity: SHA-256 and size against the manifest; no signature check yet.**
     - Fetch `{base_url}/{version}/manifest.json` and require `manifest.version == version`.
     - Stream the binary through SHA-256 with a hard cap at the manifest's `size`; it must equal both.
     - This is the integrity Claude Code's own updater has whenever its server-side signature enforcement is off
       (`manifestSignatureEnforcement: "flag"`). The manifest's signature is **not** verified: the raw-signature
       format and key are not documented publicly, so checking it would copy constants out of a vendor binary, and
       OpenPGP for the `.sig` sidecar is outside the Go standard library and `x/crypto` (AGENTS.md). Open item
       **K-14**; LIMITATIONS says "checksum-verified, not signature-verified". Anyone who controls the release host
       or `base_url` controls the binary, the same as for Claude Code's own updater.
  4. **An immutable cache, with GC.**
     - Layout `<data_dir>/harness/claude-code/<version>/claude` (0755 inside the 0700 data dir) plus `verified.json`
       (`version`, `platform`, `sha256`, `size`, `manifest_sha256`, `fetched_at`).
     - A download goes into a temporary directory, is verified, `fsync`ed and renamed into place. A version directory
       never changes after the rename. A failed check deletes the temporary directory and raises a node event, so
       nothing partial is ever executable. A per-version singleflight and a `flock` serialize downloads.
     - At start the agent re-checks size and SHA-256 of the versions it will use against `verified.json`.
     - **Tamper check at launch.** When a launch acquires its version, the node compares the binary's stat
       fingerprint (device, inode, size, mode, ctime) with the one recorded when it last hashed the file, at install
       or at the startup check. It re-hashes only when the fingerprint changed, and removes and re-downloads a version
       that fails. **Accepted:** runs share the node's uid, so a same-uid process can still replace the binary between
       that check (`Acquire`/`HoldCached`) and the exec. The window includes the launch preparation done after the
       hold: building the launch and writing its files. A same-uid process can already change much else the node
       runs, and re-hashing every launch wouldn't close the window either.
     - GC keeps the newest `cache_keep` (default 3), every version a profile seen by the node pins, and every version
       a live or adoptable run in the journal uses. Nothing goes under `~/.local`.
  5. **Absolute-path launch with the updater off.**
     - The node passes the cached binary's **absolute path** as the harness binary, so the core's `PATH` no longer
       matters.
     - `Claude.Build` sets **both `DISABLE_AUTOUPDATER=1` and `DISABLE_UPDATES=1`** in the run's (non-secret,
       hc-persisted) environment. The spike showed that `DISABLE_AUTOUPDATER=1` stops the background updater, but an
       explicit `claude update` (which an agent can run from its shell) still installs a native copy into `HOME` and
       switches `installMethod` to `native`. `DISABLE_UPDATES=1` refuses that too ("Updates are disabled by your
       administrator"). The launch shim's `inherited()` must keep not scrubbing these names.
     - A profile's `claude_settings` `autoUpdatesChannel`, `minimumVersion` or `env.DISABLE_*` have no effect on the
       version under managed mode; nothing launches a self-updated copy. The node warns when a profile sets them.
  6. **The `manage: false` escape hatch.** `harness.claude.manage: false` keeps today's behaviour (`claude_binary`
     through `PATH`) for operators who install Claude Code themselves. The node still sets both updater variables
     unless `harness.claude.self_update: true`. A profile pin that differs from the detected version then **fails the
     launch** ("this node does not manage Claude Code versions"); it never silently runs another version. Central
     likewise never sends a pin to a node below protocol minor 11: such a launch fails with a clear error.
  7. **Live runs keep their binary.** A version change affects new runs only. A run's version directory is protected
     from GC while the run is live or adoptable (and on Linux a deleted file's inode lives on for its process anyway).
     herdr-core has no method that re-executes a terminal with a new binary, so moving a conversation onto a new
     version would mean stopping the run and launching it again with `--resume`. The spike shows that works from
     2.1.317 to 2.1.322 in print mode; an automatic `on_update: resume_when_idle` policy for control runs is left out
     of this decision.
  8. **Reporting.**
     - `RunReport.harness_version` and `harness_requested`, sent with the first `starting` report, stored on the run
       (`runs.harness_version`) and shown in the run detail and API. Adopted runs take the version from the agent's
       journal, which gains a column.
     - The manifest stays the configuration resolved at creation: `dependencies["claude-code"]` becomes
       `{requested, node_default}`; the run row holds what ran. Pre-11 nodes keep today's string.
     - A new node→central `node.capabilities` message carries the full capabilities when the cache or a channel
       resolution changes, and at least every 30 minutes. `HarnessInfo` gains `managed`, `channel`, `resolved`,
       `cached` and `cache_bytes`; `version`/`path` then mean "what a new run would get now". Unmanaged nodes re-run
       detection on the same timer. Central stores a report only from the node's current connection; migration
       0049 adds `nodes.capabilities_updated_at`, shown with a stale hint after an hour (CC-5).
  9. **Protocol minor 11 and migration 0048.** Minor 11 adds `LaunchPayload.harness_version`, the two `RunReport`
     fields, `node.capabilities` and the `HarnessInfo` fields, all additive. Migration 0048 adds
     `harness_profiles.harness_version` and `runs.harness_version` (`TEXT NOT NULL DEFAULT ''`).
- **Alternatives:**
  - **Leave installation to configuration management** (a node-wide pin plus `DISABLE_AUTOUPDATER`). It is the
    interim, but it can't pin per profile and its version report stays stale.
  - **Let Claude Code update itself** (`~/.local/bin` first in `PATH`). Every run downloads, concurrent runs race,
    the version can change between two runs of one task without anyone deciding, and a profile's
    `autoUpdatesChannel` would steer every run sharing the `HOME`.
  - **Verify the manifest signature now.** Rejected until the key and procedure are documented (K-14).
- **Consequences:**
  - The node makes outbound HTTPS requests to one host and runs what it downloads. That is a new trust in the release
    host (and, if set, the operator's mirror), bounded by the checksum, the size cap, the version check and the
    fixed path shape; THREAT_MODEL gains a row when the downloader lands.
  - About 340 MB per cached version (2.1.322 linux-x64), so about 1 GB by default; the cache size is reported.
  - Runs stop writing self-updates into the agent's `HOME`.
  - Evidence so far is the contract and the spike (`docs/contracts/harnesses.md`, Distribution); none of the
    decision is implemented yet (SM-HARN-005..008 are `todo`).

- **Implementation notes (CC-4, 2026-11-13):**
  - Migration 0048 adds the pin to `harness_profiles.harness_version` (decision 9 now names that table). Runs gain
    `harness_version`, `harness_requested` and `harness_fallback`, and run reports carry all three
    (`harness_fallback` is added to decision 8's fields).
  - On an unmanaged node a channel pin (`latest`, `stable`) also fails, because such a node can't resolve a channel.
    An exact pin that matches runs by the absolute path that was checked, so the core's `PATH` can't substitute
    another binary.
  - The node learns pins only from launches. GC keeps the exact versions that profiles pinned on runs the node
    started (in this process, or in the journal) in the last 30 days, at most `harness.claude.cache_keep_pinned`
    (default 5) of them, the most recently launched. A launch that failed before its run started doesn't count.
    Versions in use by live or adoptable runs are always kept and don't count against the cap (PR #43 review S1).
  - The `{requested, node_default}` manifest dependency is for managed Claude Code only; unmanaged Claude Code and
    other harnesses keep the plain string. Central stores a reported `harness_requested` only when it is a valid
    request and `harness_fallback` only when it is a known reason, and drops anything else (PR #43 review S2, N2).
  - An API update that leaves `harness_version` out keeps the stored pin, so an older Terraform provider never
    clears it; `""` clears it.

### ADR-0078 — Changes must not fan out: no-op run reports, lean run lists, a live change feed, coalesced and cached UI queries, progress-bounded responses
- **Status:** Accepted, 2026-11-13 (incident: nodes lost their connection every 15 s after central restarted while a
  web UI tab was open; the operator stayed on v0.1.1 until the fix).
- **Context (code, at v0.1.2; v0.1.1 behaves the same):**
  - A node's hello reports every run in its journal, ended ones included; the journal is never pruned. Central applied
    each report in its own write transaction, always bumping the run's version and writing a change-feed entry, so a
    node of 2500 runs emitted 2500 `run` changes per connect. The apply took about 9 s on a disk, with the session
    reading nothing from the node meanwhile.
  - The web UI invalidated a space's runs list on every `run` change. Invalidation cancels the fetch in flight and
    starts another, but the runs query ignored the abort signal, so the cancelled request stayed open. A tab ended up
    holding hundreds of `runs?limit=200` requests it never read.
  - Each listed run carried its manifest and launch prompt and system text: a list of 200 runs with a realistic role
    prompt was 2.2 MB. Central kept each response until the client read it; there was no write deadline.
  - The memory and TCP-buffer pressure clamped every socket's receive window, including the nodes'. A node's write
    that could not complete in 15 s closed its connection; the node reconnected and the cycle started again.
- **Decision:**
  1. A run report that would change nothing stored (only `observed_at` would move) writes nothing: no version bump and
     no change entry. Fencing, real transitions, and a stop for a run central wants stopped are unchanged. The
     stale-run sweeper relies on the hello's run IDs, not on version bumps. A hello stops applying once its
     connection has ended; the next hello reports every run again.
  2. Run lists leave out `manifest`, `launch.prompt` and `launch.system` unless `include=manifest` or `include=prompt`
     asks for them. A single run's GET is unchanged. This is an API change, listed under **Changed**.
  3. The web UI collects change-feed invalidations for 250 ms and applies each distinct key once with
     `cancelRefetch: false`. Query functions that can be superseded pass the abort signal to `fetch`.
  4. Every response write must make progress: each piece of at most 32 KiB gets `http.write_idle_timeout` (default
     1m, 1s to 10m) to reach the client. A client that stops reading is disconnected. The bound is on progress, not
     the whole response, so no route is exempted: downloads, exports, backups, attachments, recordings and event
     streams that keep moving are unaffected. WebSocket upgrades (nodes, terminals) are passed through untouched; they
     keep their own timeouts.
  5. A fresh tab doesn't replay history. `GET /watch?live=1` without a position starts at the newest change and sends
     a first `position` event (with its id, so EventSource resumes from it). The tab refreshes its active queries once
     on that event, which covers what changed between loading its data and the position. Without `live=1` the feed
     still replays from the start (the CLI's `spaceman watch` relies on it).
  6. Cached queries are fresh for 60 s (was 5 s): the change feed is the freshness signal, so mounts and navigation
     reuse the cache. Every query function passes TanStack Query's abort signal to its requests.
  - Measured: with 60 space and 60 human-action changes in the history, a fresh tab visiting the inbox, overview,
    terminals and spaces pages made 318 API requests (122 `human-actions`, 64 `spaces`, 62 `system/restore`) before
    and 21 after (`web/e2e/ui-request-budget.spec.ts`).
- **Consequences:** an idle node's reconnect costs central one read per journaled run and no writes. A list of 200
  runs is about 90 KB. A tab's change-feed refetches are bounded by the window, not by the change rate. A stalled
  reader costs at most a minute of one response. Pruning the node journal or slicing the hello is a follow-up.
- **Not a security-relevant change:** nothing is relaxed. Responses are bounded more tightly, and lists return less.

### ADR-0079 — Usage limits pause runs, keep their sessions and hold the account until reset (security-relevant change)
- **Status:** Proposed, 2026-11-13. The operator answered the design's questions Q1–Q9 on 2026-11-13 by accepting
  every proposed default (decision 10, K-15). Not implemented: SM-RUN-004..006 are `todo`. Labelled
  security-relevant not because it relaxes a requirement but because **content of a run's own transcript (untrusted)
  now drives central scheduling**: it can park the run and hold an account.
- **Context:**
  - A claude.ai subscription has a 5-hour and a weekly window. When one is exhausted, Claude Code ends the turn and,
    for waits under 24 h, arms its own timer and continues about a minute after the reset (contract
    `docs/contracts/harnesses.md`, "Claude Code usage limits", 2.1.317–2.1.323). On SpaceMan runs that works only while
    nothing ends the process: control's idle stop, control replacing a worker that looks stuck, a node restart (run
    `lost`, ADR-0074) or control relaunching into the same limit. Of 21 armed waits in the examined transcripts, 16
    fired; weekly waits (more than 24 h out) never arm at all.
  - **The signal is in typed transcript fields, never the screen** (SM-RUN-002):
    - **Error path:** an assistant record with `apiError: "usage_limit_reached"`, `quotaLimits.resetsAt` (epoch
      seconds) and `rateLimitType` (`five_hour`, `seven_day`, …).
    - **Wrap-up path:** a meta user record `usageLimitNote: "wrap_up"`, then the auto-continue arm record; **no reset
      time and no kind** anywhere. Twelve of the 21 waits came this way.
    - **Cleared:** the "reset" informational record and the `auto-continuation` turn, or any later assistant message
      with usage.
  - Gateway runs (Harbor Pilot, `ANTHROPIC_AUTH_TOKEN`) never arm auto-continue. When every candidate account is
    exhausted, HP answers 429 `rate_limit_error` "no eligible account is available for model …" **without
    `Retry-After`**, and Claude Code records it as a plain API-error message with that text and no typed kind or reset
    (contracts `harbor-pilot.md`, "Quota board and the all-exhausted 429", and `harnesses.md`, "Gateway runs"). A
    blocking budget's 429 differs only by its message. The reset can be estimated from HP's `GET /api/v1/quota/board`
    `account_health[].until`, which has no project filter.
  - A typed SpaceMan nudge while armed does not cancel the timer, but its request is refused at once and the nudge is
    not acted on until the reset (contract, 4 cases). A process that exits or is relaunched loses the timer; a session
    resumed with `--resume` needs a prompt to continue.
  - `--resume` of a session under a different token works (live, fake API, 2.1.323); with two real claude.ai accounts
    it is **not verified** (K-16).
- **Decision:**
  1. **Detection on the node.** On the OSC 7501 status turning `idle` or `error`, and once at run end, the node agent
     tail-scans the run's transcript from a persisted offset and reads only typed fields: `type`, `subtype`, `error`,
     `apiError`, `apiErrorStatus`, `quotaLimits.{resetsAt,rateLimitType,status}` (falling back to
     `apiErrorParams.rate_limit_info`), `usageLimitNote`, `origin.kind` and `timestamp`.
     - **Limit set** by a `usage_limit_reached` record, or an auto-continue arm record (wrap-up path), newer than the
       last typed user turn.
     - **Gateway limit set** by an API-error record with `apiErrorStatus: 429` whose text, after the fixed
       `API Error: Request rejected (429) · ` prefix, starts with HP's `no eligible account is available for model `
       (`gateway_quota`) or matches `budget <name> is exhausted ` (`gateway_budget`). Patterns are anchored and come
       from the HP contract; anything else is not a limit.
     - **Limit cleared** by the reset informational record, an `auto-continuation` turn, or a later assistant message
       with non-zero usage.
     - No screen text and no display wording is ever parsed for status or time (SM-RUN-002); the arm record counts by
       its record type, not its text.
     - **Amended 2026-11-13 (UL-3):** the arm and the reset informational records share their `type`, `subtype` and
       `level` and differ only in wording, so neither is used. The wrap-up path is keyed on the typed meta record
       `usageLimitNote: "wrap_up"`, which comes first in the same turn; the replies the turn finishes with on its grace
       allowance don't clear it, and a later turn's reply with usage or an `auto-continuation` turn does. A typed turn
       alone doesn't clear a limit (a nudge while armed is refused at once). While the limit stays open, `since` stays
       that of the first record; kind and reset follow the newest.
  2. **The reported limit.** `RunReport.limit {kind, resets_at, since, cleared}` (protocol minor 12,
     `MinorUsageLimit`). `kind` is from an allowlist (`five_hour`, `seven_day`, `seven_day_opus`, `seven_day_sonnet`,
     `seven_day_overage_included`, `overage`, `gateway_quota`, `gateway_budget`, otherwise `unknown`; the wrap-up path
     reports `unknown`). `since` is bounded to `[now − 8 days, now + 5 min]` (missing, or further ahead: now; the 5 min allow a node clock slightly ahead, so repeats compare equal); `resets_at` is kept when it lies in
     `(since, since + 8 days]`, even if it has already passed, otherwise null; a report with neither a kind nor
     `cleared` is not a limit (`proto.RunLimit.Normalized`). **No transcript text crosses the wire or reaches a log.**
     A node below minor 12 never reports limits, and central drops a limit on any report from a session negotiated
     below 12 (`RunReport.ForMinor`), so everything behaves as before.
  3. **Keep-alive for short waits.** When the reset is known and within `keep_alive_max` (default 6 h), or unknown on
     a subscription run, the process stays up and Claude Code continues by itself. Meanwhile the run is
     *limit-waiting*: it is excluded from the **space** limit but still counts against the **node** limit (Q4);
     SpaceMan's idle-gated notices are held (the Notifier skips it and `QueueNotice` refuses idle-gated input; operator
     force-input still works) and delivered after the clear; control's idle stop is not armed for a limit-waiting
     control run.
  4. **Park for long waits.** A wait beyond `keep_alive_max`, an unknown subscription reset still open after
     `keep_alive_max`, an unknown gateway reset after 30 min, or a process that ends or is lost while limit-waiting,
     ends the run with `end_reason = 'usage_limit'` and writes a durable **limit wait** in the same transaction. Its
     resume is a **new run** of the same instance under the current lease generation with `--resume <session>`
     (when the old run had a turn) and a fixed prompt telling the agent the limit reset and to continue. Ended runs are
     never reopened, so fencing, the sweeper and the single-control index are unchanged.
     Since UL-4, central records `end_reason = 'usage_limit'` (also on `run_ended` and in the end-reason metric) for a
     run that ends while its limit is open, unless central had asked it to stop (`desired = stopped`) or the stale-run
     sweeper ended it; a later `cleared` report doesn't change it. The `run_ended` event keeps the node's own reason as
     `node_reason`. The limit wait itself comes with parking (UL-7).
     - **Amended 2026-11-14 (UL-7).**
       - **When.** A live run at its limit is parked when its known reset is more than `keep_alive_max` after its
         `since` (at once), when a gateway limit has no reset after 30 min, and otherwise when the limit is still open
         `keep_alive_max` after its `since`.
       - **How.** The limit-wait job stops the run (interrupt, `desired = stopped`) and writes its wait in the same
         transaction. The run's later end is recorded as `usage_limit`, because the run has a wait.
       - **Ended, lost or swept runs.** A run that ends, is lost or is swept while at its limit, and that nobody asked to
         stop, gets its wait in the report (or sweep) transaction. A swept run keeps the sweeper's end reason.
       - **Kinds.** Only control, worker and diagnostic runs are parked (control's decision, 2026-11-14). KB support and
         dreamer runs are short-lived and started on demand by their schedulers, which start them again when needed,
         so they are neither stopped at the keep-alive nor resumed.
       - **What the wait records.** The session (only when the run finished a turn), the task (only while it is assigned
         to the run and running, blocked or in review), the space's open hold, and the run's launch.
       - **Re-park.** When a resumed run parks again, the new wait carries its predecessor's `release_at`, `attempts`
         and original prompt forward, so the maximum wait and the backoff don't start again. An active wait another run
         of the same instance left is superseded (cancelled).
  5. **Resume job.** A deadline-timer job resumes due waits at `resets_at` + 2 min (unknown: retried with backoff from
     1 h, each attempt re-detected and re-parked without new notices). A paused space, a still-open account limit, a
     node with no lease or a full space keeps the wait waiting; node hello kicks it. An open hold with an **unknown
     reset** does not block its own retry: it is **probed** at the hold's `usage_limits.check_at` (decision 7), and
     the retry that the probe lets through is that attempt. Resumes go through `Admit`; when
     several are due, control first, then the oldest task, and a resume never pre-empts a launch already admitted
     (Q6). A space that relocated resumes without a session, and control is told.
     - **Amended 2026-11-14 (UL-7).** The job is the `limit_waits` deadline on central's shared timer, batched and
       bounded after the 2026-11-13 incident.
       - **A round.** It recovers stuck resumes, releases waits past their maximum wait, parks runs that are due, and
         resumes due waits: control first, then the oldest, at most 5 per round. When more are due, the next round
         waits 10 s.
       - **No writes when nothing is due.** A round with nothing due only reads (indexed). A wait put off again for the
         same reason at the same time writes nothing.
       - **Resume time.** The reset plus 2 min. With an unknown reset, the hold's `check_at` (the T6 probe, which the
         resume claims in its launch transaction, like any launch). With no hold, back off from 1 h, doubling with each
         attempt, capped at `keep_alive_max`.
       - **Checks.** Each resume claims the wait (`waiting` → `resuming`) after reading:
         - the parked run must have ended; until then the wait retries in 2 min (`run_not_ended`);
         - the space: archived gives the wait up; paused retries in 1 h; relocating retries in 2 min;
         - the task must still be the run's; otherwise the wait is cancelled (`task_moved`);
         - no other live run of the instance may exist (`superseded`);
         - the lease: no valid lease retries in 15 min (`node_offline`).
       - **Launch refusals.** A held account defers the wait to the hold's free time. A full space retries in 2 min; a
         resume never pre-empts another run. A control conflict cancels the wait. Any other refusal retries in 10 min.
         All of these are fixed reasons, never raw text.
       - **One transaction.** The launch inserts the new run (same instance, current lease generation,
         `LaunchPayload.Resume` with the session) and marks the wait `resumed`. It re-points the task through the
         queue, and tells control. So a wait still `resuming` never started a run: the first round of a process, and
         later rounds for rows resuming longer than 10 min, return such waits to `waiting` (UL-2 review follow-up).
       - **Relocation.** It is detected by the space's node differing from the wait's. The run then starts a fresh
         session with its original prompt and a fixed note; control's message says so, or a resumed control's own
         prompt does.
       - **Brought forward.** The node's hello (after its leases are granted) and a space's resume bring waits put off
         for that reason forward; they read first and write only when such a wait exists.
       - **The parked run** is set to `desired = stopped` in the claim, so a parked control releases the single-control
         slot even if the supervisor didn't settle it.
       - **Auto-start** leaves a space whose control has an active wait alone; the wait brings control back.
       - **Not done.** The probe isn't reserved for a resume over a plain launch: the first launch at `check_at` takes
         it, as in UL-6. The deadline job doesn't clear holds whose reset passed: such holds no longer refuse anything.
       - **PR #59 review.**
         - **A hold that clears wakes its waits.** Clear-now, an account clear, evidence, or a probe that made progress
           sets `resume_at = now`, in the clear's transaction, for two kinds of waiting wait: those parked under the hold
           (`usage_limit_id`), and those put off as `account_held` whose launch uses the hold's account (or gateway). The
           job resumes them on its next round; the clear launches nothing.
         - **The launch transaction re-checks** that the instance has no other live run. One started between the claim and
           the launch supersedes the wait (`superseded`).
         - **A failed `--resume`.** A resumed run that ends `failed`, or `exited`, before its first turn (the resume
           didn't work) gets a new wait due at once. It carries no session and the original prompt (the fresh-session
           path), and control is told once (`limit-wait-resume-failed:<wait id>`). Only a resume that used a session gets
           this, so a wait has at most one fresh retry.
         - **The supervisor** neither restarts nor counts a control that has a limit wait (swept at its limit, or a failed
           resume).
  6. **Release.** A worker's wait is released after `max_wait` (default 7 days, Q2): the task returns to `ready`
     with its assignee cleared and control is told. A control wait is never released.
     - **Amended 2026-11-14 (UL-7).**
       - **The setting.** `max_wait` is the space limit `usage_limit_max_wait_hours` (1–720, default 168), following
         UL-6's settings. It applies to worker and diagnostic waits parked after it changes. Control's wait has no
         `release_at`.
       - **The release.** One transaction marks the wait `released` (`max_wait`). The queue sets the task `ready` and
         clears `assignee_run`, only while it is still the parked run's, with its outbox event and audit. Control is
         told once (dedup `limit-wait-released:<wait id>`); the queue's own "task ready" notice isn't sent.
       - **A task in review** (PR #59 review) stays in review on a release, with only its assignee cleared; control's
         message says it still waits for acceptance. The message says "back in the queue" only when the task really
         went back to ready.
       - **Task changes** (UL-2 review follow-up). A task that becomes done, failed, cancelled, ready or backlog, or
         is assigned to another run, cancels its waiting waits (`task_moved`) in the same queue transaction, through
         the `limit_waits(task_id)` index. An incident's preemption, which returns running tasks to ready, does the same.
       - **Operator actions.** `POST /spaces/{id}/limit-waits/{wait_id}/resume` resumes a wait now, whatever its resume
         time. The usual checks apply; a refusal returns the reason and leaves the wait as it was.
         `POST /spaces/{id}/limit-waits/{wait_id}/release` gives a wait up now; control's too, after which only
         auto-start brings control back. Both need `run:control` in the space, like clear-now, are audited
         (`limit_wait.resumed`, `limit_wait.released`), and have CLI commands (`spaceman limits resume`,
         `spaceman limits release`).
  7. **Account hold.** A limit report opens a `usage_limits` row for the run's resolved account (or gateway
     reference). While it is open, a launch or resume on that account is refused in the launch transaction (a DB read
     only) with `CodeRateLimited` and the reset time in UTC. The hold applies **to every space bound to that account**;
     control is notified per space (Q8). Control autostart is deferred silently to the reset, and the supervisor
     neither restarts nor counts a control run that ended `usage_limit`. The hold clears when `resets_at` passes, a
     node reports `cleared` on any run of the account, any other run on the account makes progress (an assistant
     message with usage), the quota board shows a gateway account available, or an operator clears it.
     - **Unknown resets are probed, so a hold never deadlocks** (amended 2026-11-13, UL-2 review). A hold without
       `resets_at` refuses everything except a **probe**: at the hold's `usage_limits.check_at`, exactly one resume
       (or, with none waiting, one launch) on the account is let through. A probe that makes progress clears the
       hold; one that hits the limit again moves `check_at` on with backoff (from 1 h, doubling, capped at
       `keep_alive_max`). The gateway re-check of decision 9 uses the same column.
     - **Scope of a hold** (amended 2026-11-13, UL-4). A direct run holds its resolved account (`scope = 'account'`).
       A Harbor Pilot run holds a gateway reference whose granularity follows the cause: a quota exhaustion (`gateway_quota`,
       or any kind other than a budget) is connection-wide, `gateway_ref = 'harbor-pilot'`, because no account behind the
       one connection is eligible; a blocking budget (`gateway_budget`) belongs to one project,
       `gateway_ref = 'harbor-pilot:project:<project id>'`, and holds only the spaces charged to it. A report opens a
       row in the reporting space only. Every other space that binds the account (or routes through the same connection or
       project) sees it as a **shared** hold, without the other space's identifiers. An unknown reset gets
       `check_at = now + 1 h`; a later known reset fills the row in and drops the probe.
     - **Model-specific weekly limits hold the whole account** (control's conservative default, 2026-11-13):
       `seven_day_opus` and `seven_day_sonnet` hold the account for every model, not only the exhausted one.
     - **Clearing a hold across spaces** (control's decision, 2026-11-13). Accounts are global objects, so "clear
       now" on an account hold clears **every open row for that account and kind in all spaces**, and needs
       **account-level permission** (the same as managing accounts). An operator with only space-level permission
       clears **their space's row only**: that does not lift the hold in other spaces (the hold is read across
       spaces), and the UI says so. Every clear is audited; each other space whose row was cleared has its control
       told once.
     - **Evidence clears** (UL-4, PR #55 review). A node's `cleared` report is evidence that the account works again.
       It is separate from the operator's "clear now" above, whose permission rule it doesn't change.
       - The clear names the `since` of the limit it ends. A clear of an older `since` than the run's stored limit is
         late (the node repeats it on every later report) and changes nothing. A newer limit (another `since`) also
         ends the stored one.
       - Only rows of that kind first seen no later than the ended limit's `since` count. So a new window always gets
         a row of its own: an open row whose reset passed before the new limit was seen is cleared, never reused.
       - The same limit reclassified (the same `since`, a newer record of another kind, such as the wrap-up path's
         `unknown` then a typed kind) supersedes the hold the run opened for the old kind.
     - **Trust decision: nodes are trusted for their own runs; cross-space effects are limited to scheduling a probe.**
       - The run's own space's rows are cleared, with `cleared_by = run:<id>`.
       - Another space's row is cleared only when its reset has passed (within 2 min) or is unknown. It gets
         `cleared_by = evidence` and is correlated on the hold, so that space never sees this space's run.
       - A row whose reset is still ahead gets `check_at = now`: one probe launch decides.
     - **Events and visibility.**
       - Each cleared row publishes one `fleet.usage_limit.cleared` event in its own space, and each opened row one
         `fleet.usage_limit.reached` event.
       - A report that changes nothing writes nothing.
       - A space's listing shows other spaces' open holds as one shared row per account (or gateway reference) and
         kind, so it doesn't reveal how many spaces hit the limit.
     - **At launch** (amended 2026-11-13, UL-6). A hold whose reset has passed no longer refuses, whether or not the
       deadline job has cleared it yet. A hold due for its probe lets the launch through and is claimed in the same
       transaction: an unknown reset's `check_at` moves on by the time since the hold was seen, at least 1 h and at most
       `keep_alive_max` (so 1 h, 2 h, 4 h … from the first probe); a known reset's `check_at` is set to none (one probe,
       then the reset decides). Any hold that is neither refuses, and the refusal writes nothing. Its text names the
       account (or the Harbor Pilot connection or project) and the time the last hold lets a launch through, never
       another space. Until UL-7 adds resumes, the probe goes to the first launch.
     - **Control** (UL-6). A control restart or auto-start refused by a hold defers auto-start until that time; later
       sweeps only read the hold, so a clear (operator or evidence) lets control start at the next sweep. Nobody is asked
       to look.
     - **Clear now** (UL-6). The space-level clear needs `run:control` in the space (the permission that launches runs
       there; operators and admins) and returns `held_elsewhere`; the account-level clear needs `account:write` and takes
       an optional kind (none: every kind, as the opus/sonnet holds hold the whole account). Control is told with dedup
       key `usage-limit-cleared:<usage_limits id>`, the key decision 11's clear message uses, so one hold is announced
       once.
  8. **Failover is opt-in.** With the space setting `usage_limit.failover = true` (default **false**), `Resolve` picks
     the next bound account for the same harness without an open limit, for launches and resumes. SpaceMan never falls
     back from a gateway binding to a direct account (Q1). (UL-6: the settings `failover` and `keep_alive_max` are the
     space limits `usage_limit_failover`, a switch, and `usage_limit_keep_alive_max_minutes`, 1–1,440, default 360, so
     they get installation defaults, per-space overrides, audit and the limits API with no new table. Failover avoids an
     account with any open hold whose reset hasn't passed, a probe-due one included; the default comes first, then
     binding order; with every account held the usual choice stands and is refused.)
  9. **Gateway waits.** The reset is an **estimate**: the earliest `until` among the quota board's unavailable
     accounts (quota), or the covering blocking budget's `period_end` (budget). The board is re-read every 15 min,
     cached for 60 s, never inside a transaction; HP's `quota.window.recovered` event may kick the job early (Q9). The
     re-check is scheduled by the hold's `usage_limits.check_at`, as is the probe of decision 7.
     - **Amended 2026-11-14 (UL-8).**
       - **The job.** `gateway_waits` runs on central's shared deadline timer. It reads every open gateway hold of every
         space (indexed, at most 200). A hold without an estimate yet is read at once; an estimated one at its
         `check_at`. One board read serves the quota hold of every space, and one budget read serves each project.
         Reads use the connection's service-principal token outside any transaction. The board is cached for 60 s;
         budget status keeps its own 30 s cache, shared with the launch check.
       - **The estimate.** Quota: the earliest future `until` among the board's unavailable accounts. Budget: the
         latest `period_end` among the covering blocking budgets. It is stored as the hold's `resets_at`, with
         `source` `quota_board` or `budget_status` and `check_at = now + 15 min`. Waits parked under the hold are
         re-timed to the estimate plus the 2-minute slack. A gateway wait put off as `account_held` is moved only
         earlier. An estimate the hold already has writes nothing (no version, `check_at` or change event); the job
         keeps the next re-read in memory.
       - **An estimated hold's `check_at` is the re-read, never a probe.** The hold refuses launches until its
         reset, and the refusal says "until about `<time>` (estimated from Harbor Pilot's quota board …)" or "until
         `<time>`, when the budget's period ends". The probe of decision 7 stays in charge while Harbor Pilot names no
         reset: the board lists no account at all (an empty, missing or renamed `account_health` is no evidence, never
         a recovery), every unavailable account has a null or past `until`, a blocking one-time budget has no end, a
         blocking budget's `period_end` has passed (Harbor Pilot hasn't rolled the period yet), or Harbor Pilot can't
         be read. Such a hold is read again after 15 minutes, or after 2 minutes when a read failed or a period end
         has passed. A
         node's evidence from another space sets `check_at = now` as before. On an estimated hold that means "read
         again now".
       - **Recovery.** The board lists every account of the installation, including ones the space's plan never
         routes to. For example, the demo's local model has no monitor and is always available. So "any account
         available" is no signal. With each board estimate the job remembers the accounts behind it (those whose
         `until` is the estimate), in memory, for the hold's id and version. A quota hold clears when no account is
         unavailable any more, or when one of those accounts is available again. An account behind the estimate that
         is still out with a moved `until` re-times the hold to the new earliest `until`; it never clears it. When the
         job doesn't know the accounts (after a restart, or the hold changed meanwhile), it only re-estimates. A newly
         exhausted account with an earlier `until` moves the estimate earlier. A budget hold clears when no covering
         budget blocks any more. The clear has `cleared_by` `quota_board` or `budget_status` and wakes the waits, as
         other clears do.
       - **Kick.** `events.Options.OnAccepted` is called after an event becomes a new item. When the subscription
         uses the built-in `harbor-pilot-quota` template and the subject is
         `evt.harborpilot.quota.window.recovered`, central drops the cached board and makes every quota hold due.
         A board read already in flight when the kick arrives is neither cached nor used: it is read again. The
         event is untrusted and only brings the read forward. Without such a subscription the 15-minute re-check
         alone applies.
       - **A gateway run parked after its hold cleared** resumes at once, not after the 1-hour backoff. The gateway hold
         opens with the limit, so a missing hold means Harbor Pilot recovered, or the hold was cleared. A run still
         kept alive when the hold clears is parked only at its 30-minute keep-alive (not changed here).
       - **No migration.** `usage_limits.source` already allowed both sources (0050). The protocol is unchanged.
  10. **Operator answers (2026-11-13, all proposed defaults accepted).**
      - **Q1 failover:** off by default, a per-space setting; no gateway → direct fallback.
      - **Q2 maximum wait:** 7 days for a worker's task, then released to the queue; control is never released.
      - **Q3 keep-alive versus park:** keep alive up to 6 h, park longer waits.
      - **Q4 node capacity:** a kept-alive run counts against the node limit, not the space limit.
      - **Q5 people:** one human action (type `message`, priority `normal`) to the affected space's operators, once per
        account window, only for waits over 12 h.
      - **Q6 priority:** control first, then the oldest task; a resume never pre-empts a launch already running.
      - **Q7 early stop:** a later, optional step (T13, a separate amendment): stop new launches on an account at
        ≥ 95 % of a window, from a SpaceMan-owned statusline. Not part of this decision.
      - **Q8 cross-space:** the hold applies to every space bound to the account.
      - **Q9 gateway estimate:** accepted (earliest reset, re-checked every 15 min); no per-project board view requested.
  11. **Telling control and people once.** Control gets one durable message per space and account window when a
      limit opens and one when it clears or resumes (dedup keys from the `usage_limits` id); worker `run-ended` notices
      for `usage_limit` ends are folded in. People get the single action of Q5.
      - **Amended 2026-11-13 (UL-5, control's decision):** the keep-alive messages are per **run and limit window**:
        one "at its usage limit" and one "usage limit cleared" per (run, `since`), written inside the report
        transaction that changes the run's limit window, never per report. A "cleared" message goes out only for a
        window control was told about. Control isn't told about its own limit, and a run that ends at its limit is
        reported by its run-ended notice. The per-account-window message with the parked runs and the folding of
        run-ended notices come with parking (UL-7).
      - **Amended 2026-11-14 (UL-7):** parked runs are announced **per wait**, written in the transaction of the change:
        one "parked" message (`limit-wait:<wait id>`) with the resume and release times, one "resumed" message
        (`limit-wait-resumed:<wait id>`), and one "given up" message (`limit-wait-released:<wait id>`). They replace
        the run-ended notice of a parked run. A re-park, and later resumes of the same chain (an unknown reset probed
        again), aren't announced again; a fresh-session resume always is. Control's own wait isn't announced to it. A
        per-account-window summary isn't sent.
  12. **Numbers.** Protocol minor 12 (`MinorUsageLimit`); central migration 0050 (`runs.limit_kind` without a CHECK,
      so a new kind never needs a runs-table rebuild, `limit_resets_at`, `limit_since`; tables `usage_limits`, with
      `check_at` and the kind CHECK, and `limit_waits`); agent
      migration 0008 (`runs.limit_kind`, `limit_resets_at`, `limit_since`, `limit_offset`; `limit_since` added in UL-2
      so a restarted agent re-reports the same `since`). Space setting
      `usage_limit {failover, keep_alive_max, max_wait, notify_humans_after}`.
      **Amended 2026-11-14 (UL-6, UL-7):** these are space limits, not a settings object: `usage_limit_failover`
      (switch, default off), `usage_limit_keep_alive_max_minutes` (1–1,440, default 360) and
      `usage_limit_max_wait_hours` (1–720, default 168: Q2's 7 days). `notify_humans_after` comes with UL-9.
- **Alternatives:**
  - **Parse the TUI's "You've hit your … limit · resets …" text.** Rejected: forbidden by SM-RUN-002, local-zone and
    dateless under 24 h, and server-overridable wording.
  - **Always park.** Simpler and frees memory, but discards in-memory state (background shells) and Claude Code's own
    timer, which already handles the common 5-hour case (Q3).
  - **A new `observed`/`waiting` run state.** Rejected: it would touch every literal live-state list, the sweeper and
    the single-control index; columns plus two tables avoid that.
  - **`StopFailure` hook or statusline `rate_limits`.** The hook carries no reset time and the statusline needs an
    unsafe settings key; kept for T13 as accelerators, not the source.
  - **Trust `Retry-After` for gateway runs.** HP doesn't send it in this case.
- **Consequences:**
  - A run's transcript can now park that run and hold its account. A forged record can block an account for at most
    8 days (the clamp); only typed fields are read, no free text is stored, the kind is allowlisted, progress by any
    other run on the account clears the hold, and an operator can clear it. THREAT_MODEL gains a row when the
    detector lands.
  - Claude Code's record format is undocumented; a rename silently stops detection and falls back to today's
    behaviour. The contract records the versions, ADR-0077's managed versions let a fixture test follow the pinned
    version, and a bounded metric counts unclassified 429 records.
  - The wrap-up path gives no reset time, so more than half the subscription waits seen run with an unknown reset:
    keep-alive covers them while the process lives, and a parked one is resumed by backoff.
  - A gateway reset is an estimate and may resume early (re-detected, re-parked, no new notice) or up to 15 min late.
  - A real subscription limit can't be forced in tests; until one is observed on a SpaceMan run, real evidence stays
    empty (K-16).

## 2. Open items and unresolved contracts

| ID | Item | Blocks | Plan |
|---|---|---|---|
| K-1 | ~~hc framed protocol byte layout.~~ **Resolved 2026-11-02:** `ch u32 LE · kind u8 · len u32 LE`; DATA=0, CREDIT=1, CLOSE=2, RESIZE=3 (spike S-3). | — | done |
| K-2 | ~~Whether hc persists `env`.~~ **Resolved 2026-11-02:** yes, plaintext `restart_env` → ADR-0009 shim required. | — | done |
| K-3 | ~~hc parent semantics.~~ **Resolved 2026-11-02:** children killed when the parent program exits or is killed, unless `outlive_parent` → ADR-0017 lease guard. | — | done |
| K-4 | ~~Whether Pulse `durable` grants accept name patterns.~~ **Resolved 2026-11-02:** exact names only; DELETE permitted → fixed name + delete/recreate (ADR-0011). | — | done |
| K-5 | ~~ghostty-web package and API.~~ **Resolved 2026-11-02:** `ghostty-web` 0.4.0 (MIT): `init()`, `new Terminal({cols, rows, theme, fontSize, fontFamily, disableStdin, scrollback})`, `open`, `write(string \| Uint8Array)`, `resize`, `reset`, `dispose`; WebAssembly is inlined (ADR-0020; since ADR-0076 the page compiles the package's `ghostty-vt.wasm` asset instead). 2026-11-03 (ADR-0046): `term.options.disableStdin` is read on every key, `onData` delivers typed input and `focus()` exists (checked in the 0.4.0 dist). | — | done |
| K-6 | ~~Official Go MCP SDK version and streamable/stdio APIs.~~ **Resolved 2026-11-02:** `github.com/modelcontextprotocol/go-sdk` v1.8.0: `mcp.NewServer`, typed `mcp.AddTool`, raw `Server.AddTool` (input schema must be an object, or it panics), `IOTransport` for stdio. Upstream client (M3-03): `mcp.NewClient` + `Client.Connect` with `StreamableClientTransport{Endpoint, HTTPClient, MaxRetries, DisableStandaloneSSE, MaxEventSize}`; the connection context is detached from the `Connect` context; `ClientSession.Tools` (paginated), `CallTool`, `Close`. Transport failures surface as JSON-RPC code -32005. | — | done |
| K-7 | ~~TipTap extension set.~~ **Resolved 2026-11-02:** `@tiptap/react`, `starter-kit`, `markdown` (official Markdown parse/serialize), `extension-image`, `extension-list` (task lists), `extension-table`, `extension-file-handler` are MIT (npm registry). Pinned to 3.32.0 (released 2026-10-13); renderer `marked` 18.1.1 (MIT) + `dompurify` 3.4.17 (MPL-2.0 OR Apache-2.0). | — | done |
| K-8 | ~~llm-knowledge-base-template layout and licence; Piebald dream prompts licence.~~ **Resolved 2026-11-02:** KB template in `docs/contracts/kb-template.md`. Piebald-AI/claude-code-system-prompts is MIT, but its files are extracted Claude Code prompts whose text the repository licence may not cover; SpaceMan's dream prompt is written from the brief's principles and copies none of it (ADR-0027). | — | done |
| K-9 | ~~Codex ChatGPT-login token materialization (`auth.json`) vs API key.~~ **Resolved 2026-11-03:** the `codex_chatgpt_login` kind, one live copy per account linked into each `CODEX_HOME` (ADR-0053); a real ChatGPT login is not verified (no account). | SM-ACCT-002 | done |
| K-11 | ~~Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26).~~ **Resolved 2026-11-02:** SSO grants are stored under `user:<id>#sso`, which only SSO sessions act as (ADR-0029); SR-26 ported as `TestReviewSR26SSOGrantsOnlyForSSOSessions` (`internal/auth`, `internal/oidc`) and `TestOIDCSignInOverHTTP`. | — | done |
| K-10 | ~~Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode.~~ **Resolved 2026-11-02:** works interactively with an empty config dir once first-run state and folder trust are seeded (spike S-5, ADR-0021). | — | done |
| K-12 | Harbor Pilot integration design questions Q1–Q7. Answered by the operator on 2026-11-06 (ADR-0060), except **Q4**: routing of unscoped HP events (quota, access-blocked, binding/workspace/global budgets) is undecided; the interim default (control's proposal) is no automatic routing, delivery only to spaces that explicitly subscribe. Q7: a real upstream is "coming soon". | SM-GW-007, SM-GW-010 | ADR-0060 accepted 2026-11-08 with Q4 recorded as open; live-run placeholder in TRACEABILITY and RELEASE_READINESS §13 |
| K-13 | ~~Harbor Pilot 0.1.1 refuses Claude Code 2.1.317's `system`-role entries in `messages[]` (seen with the default Claude 5 model; `claude-sonnet-4-5` works).~~ **Resolved 2026-11-07:** Harbor Pilot 0.1.2 accepts them (0.1.3 downgrades them only for models known not to support them, its D-042). Re-run with Claude Code 2.1.317 on its default `claude-opus-5-5` through `serve --demo` v0.1.3 with a minted child token: `["user","system"]` got 200 and the turn completed, passed through unchanged on the demo's Anthropic-shaped upstream (`docs/contracts/harnesses.md`, Harbor Pilot 0.1.3 re-run). A real Anthropic upstream is not verified (demo upstreams only). | — | done; SpaceMan requires Harbor Pilot ≥ 0.1.3 (ADR-0060) |
| K-14 | Signature verification of Claude Code release manifests. Each version has `manifest.json.raw-sig.json` (`RSASSA-PKCS1-v1_5-SHA512` over a fixed prefix plus the manifest, key compiled into Claude Code) and a detached PGP `manifest.json.sig`; neither the key nor the procedure is documented publicly, and OpenPGP is outside the Go standard library and `x/crypto`. Until then the node verifies SHA-256 and size against the manifest only (ADR-0077; LIMITATIONS, Harness distribution). | SM-HARN-006 | Verify the PGP (or raw) signature once Anthropic documents the release key and procedure; re-check on each contract update |
| K-15 | ~~Usage-limit handling questions Q1–Q9 (failover, maximum wait, keep-alive versus park, node capacity, people, priority at reset, early stop, cross-space holds, gateway estimate).~~ **Resolved 2026-11-13:** the operator accepted every proposed default; recorded in ADR-0079 decision 10. | — | done |
| K-16 | Usage-limit behaviour not verified live (`docs/contracts/harnesses.md`, "Claude Code usage limits"): `--resume` with a **different real claude.ai account** (only different tokens against a fake API were tried; the binary has an `account_switch` auto-continue cancel); whether SpaceMan's interrupt counts as `ctrl_c` and cancels the auto-continue timer; auto-continue across a node agent restart that adopts the process; the `seven_day_opus` and `seven_day_sonnet` kinds; whether Claude Code reports OSC 7501 `error` (rather than `idle` or `done`) when a turn ends at a limit (the node scans on all three, so detection doesn't depend on it). A real limit can't be forced. Also unverified: how a real `--resume` of a parked session fails (a missing or unreadable session file), which UL-7 treats as a run that ends `failed` or `exited` before its first turn and restarts once in a fresh session (simulated only). | SM-RUN-004, SM-RUN-005 (real evidence) | Failover (ADR-0079 decision 8, off by default) is checked with two real accounts before it is relied on (T6/T7); the rest is recorded when a live limit is observed on a SpaceMan run (T12). ADR-0079 never interrupts a keep-alive run and parks by ending the process, so neither design path depends on the timer questions |
