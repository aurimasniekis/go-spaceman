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
- **Status:** proposed; depends on decoding the framed protocol (open item K-1)
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

## 2. Open items and unresolved contracts

| ID | Item | Blocks | Plan |
|---|---|---|---|
| K-1 | hc framed protocol byte layout (DATA/RESIZE/CREDIT/CLOSE frames) is undocumented in `hc schema`. | ADR-0010, live terminal | Spike: proxy `hc attach` through a recording socket proxy against an isolated core; document; fallback PTY tee. |
| K-2 | ~~Whether hc persists `env`.~~ **Resolved 2026-11-02:** yes, plaintext `restart_env` → ADR-0009 shim required. | — | done |
| K-3 | ~~hc parent semantics.~~ **Resolved 2026-11-02:** children killed when the parent program exits or is killed, unless `outlive_parent` → ADR-0017 lease guard. | — | done |
| K-4 | ~~Whether Pulse `durable` grants accept name patterns.~~ **Resolved 2026-11-02:** exact names only; DELETE permitted → fixed name + delete/recreate (ADR-0011). | — | done |
| K-5 | ghostty-web npm package name, version, API. | Terminal UI | Inspect the package source before use. |
| K-6 | Official Go MCP SDK version and streamable/stdio APIs. | MCP gateway | Inspect module source. |
| K-7 | TipTap open-source extension set and Markdown serializer. | SM-ATT-003 | Inspect packages and licenses; exclude Pro extensions. |
| K-8 | llm-knowledge-base-template layout and licence; Piebald dream prompts licence. | KB, dreamer | Fetch and record; attribute or paraphrase per licence. |
| K-9 | Codex ChatGPT-login token materialization (`auth.json`) vs API key. | SM-ACCT-002 | Inspect Codex docs/behaviour; support API key first, document ChatGPT auth handling. |
| K-11 | Hallpass applies SSO-sourced roles only to SSO sessions (its SR-26). SpaceMan grants have no SSO source yet (OIDC lands in M4-01). | SM-AUTH-002 | When OIDC lands, store SSO grants under the subject `user:<id>#sso`, add that subject only for SSO sessions, and port the SR-26 test. |
| K-10 | Claude `setup-token` OAuth via `CLAUDE_CODE_OAUTH_TOKEN` in interactive mode. | SM-ACCT-002 | Verify in a real opt-in run. |
