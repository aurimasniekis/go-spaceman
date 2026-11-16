# Plan: rename a space role safely (API, CLI, admin MCP, Terraform provider)

- **Status:** planning only, with no code changes. ADR-0093 is recorded in `docs/DECISIONS.md` (Proposed). Q0–Q5 were decided on 2026-11-16 (§8). The ADR text is in §7
  (**ADR-0093**).
- **Goal:** rename a space role (for example `implementor` to `implementor-hard`) without losing its configuration,
  pending work or memory. Today the only way is to delete and recreate the role, and that strands every copy of the
  old name.
- **Base:** `next/v0.1.9` at 7a68cda6. Every **[code]** fact below was read there. Facts about the provider were read
  on its `main` at 7a2c24c.
- **Out of scope:** a rename button in the web UI (a follow-up once the API exists), renaming role templates, harness
  profiles, spaces or instances, a "delete a role's memory" operation (§8, Q4) and an "abandon the node move" action
  (§8, Q3). The last three are follow-ups.
- **Builds on:** PR #195 (memory conflict folders, ADR-0092, migration 0060), which merges into `next/v0.1.9` first and
  also changes `internal/memory/memory.go`. `memory_conflicts` is keyed by collection id, so a rename leaves its rows,
  including the new `folder` column, untouched.

---

## 1. What exists today [code]

### 1.1 The role and its name

- `space_roles` has primary key `(space_id, name)` (`internal/store/migrations/0012_catalog.sql:70`). The name is
  checked only in Go, against `^[a-z][a-z0-9-]{0,47}$` (`internal/catalog/config.go:83`). The same regex is copied into
  messages, accounts, fleet, human policy and memory; kb, dream and the gateway use a variant that also allows `*`.
- **No foreign key refers to `space_roles`.** No `REFERENCES space_roles` and no `ON UPDATE` exist anywhere, so every
  other table holds the name as plain text.
- API (`internal/api/catalog.go`): `list-space-roles`, `get-space-role`, `save-space-role` (`PUT
  /spaces/{space_id}/roles/{name}`, an upsert), `delete-space-role`, `preview-space-role` and `add-starter-roles`. All
  of them use the catalog rules map (`SpaceConfigRead`/`SpaceConfigWrite`, with no step-up).
- `DeleteSpaceRole` (`config.go:458`) deletes only that row and checks no references. Grants, pools, schedules,
  memory and messages keep the dead name. Nothing renames a role anywhere.

### 1.2 Every place the name is copied (latest schema)

**Live** means the value is read later to route, launch, authorise or deliver. **History** means it records what
happened.

| Where | Kind | Notes |
|---|---|---|
| `space_roles.name` | live | the row itself |
| `mcp_grants.role` | live | `UNIQUE(server_id, space_id, role)`; read on every gateway call |
| `kb_repos.space_role` | live | must name an existing role (`internal/kb/kb.go:322`) |
| `kb_repos.allowed_roles_json` | live | JSON array; may hold `*` |
| `dream_configs.space_role` | live | must name an existing role (`internal/dream/dream.go:216`); also the "own" role left out of memory |
| `dream_configs.memory_roles_json`, `transcript_roles_json` | live | JSON arrays; may hold `*` |
| `space_schedules.assignee_role` | live | copied into each task the schedule creates |
| `human_policies.successor_role` | live | `LiveRunOfRole` fallback routing |
| `space_human_type_permissions.agent_roles_json` | live | JSON array |
| `space_role_accounts.role` | live | per-role account pool (0054); PK `(space_id, role, account_id)`. **Not in the original list.** |
| `memory_collections.role` | live | `UNIQUE(space_id, role)`; also names the central Git repository on disk |
| `tasks.assignee_role` | live while the task is open, history after | |
| `messages.to_id` where `to_kind='role'` | live while `state IN ('stored','available')` (including a future `deliver_at`) | **not in the original list** |
| `kb_requests.requester_role` | live while the request is open | an answer to an ended requester goes to `role:<requester_role>` (`kb.go:903`) |
| `human_notifications.target_role` | live while pending | a label only; routing uses `target_kind`/`target_run`/`target_instance` |
| `limit_waits.role`, `account_switches.role` | live while open | a resume relaunches with this role (`internal/fleet/limitwait.go:1045`) |
| `node_commands.payload_json` (`run.launch` `Role`) | live until the node accepts it | |
| `runs.role`, `runs.manifest_json` | history, **read live while the run runs** | `AuthenticateRun` joins `runs` on every agent call (`internal/fleet/runs.go:1194`) |
| `human_actions.requester_role`, `approvals.requester_role`, `xspace_requests.requester_role`, `kb_sessions.requester_role` | history | |
| `run_usage.role`, `usage_buckets.role` (part of the PK) | history | rewriting could also collide in the PK |
| `inbox_items.dedup_key` (`role:<role>:` prefix), `inbox_items.source_label`, `messages.from_label`, `audit_events`, `event_outbox` payloads | history | |

Columns named `role` that do **not** hold a space role: `role_grants.role`, `oidc_group_mappings.role`,
`telegram_links.max_role`, `space_human_type_permissions.{view,create,respond}_role` and `space_repositories.role`.

**Relaunches that copy the role from a history row** (they would relaunch under the old name after a rename):
- the control supervisor's restart, `Role: r.Role` (`internal/control/supervisor.go:278`);
- control autostart from `LatestControlRun().Role` (`internal/control/lifecycle.go:90`);
- limit-wait resume (`limitwait.go:1045`).

`Resolve` with an unknown role silently falls back to no template and no prompt (`internal/catalog/resolve.go:95`), so
such a relaunch would not fail loudly. It would just run with no template and no prompt.

### 1.3 Memory [code]

- **Central:** one bare Git repository per collection, at `<data>/memory/<space>/<role>.git`
  (`internal/memory/memory.go:104`). The row in `memory_collections` is created lazily on the first sync or status
  report (`:109`). `Apply` (`:141`) holds a mutex per `space/role`, accepts only a fast-forward of the DB head (CAS),
  stores a divergence as `refs/conflicts/…` plus a `memory_conflicts` row (keyed by collection id), and asks for full
  history on `missing_base`. Backups bundle each repository to `memory/<space>/<role>.bundle`
  (`internal/backup/backup.go:605`). Relocation exports `memory-<role>.bundle`.
- **Node (where memory actually lives):** `<agent data>/memory/<space>/<role>/shared.git`, plus one checkout per
  instance at `<space>/<role>/instances/<instance>` (`internal/agent/memory.go:74`). Checkouts fetch from `shared.git` by
  explicit path, with no stored remote, so moving the role folder does not break them. The node DB has `memory_sync(space_id,
  role, acked, retries, conflicts, …)` (`internal/agent/migrations/0002_memory.sql`). On every reconnect the node finds
  collections **by directory name** (`collections()`, `memory.go:414`) and offers them again.
- **Wire:** node to central `memory.sync` / `memory.status` / `memory.flushed`; central to node `memory.ack` /
  `memory.flush`. Central never edits a node's memory, and **no central-to-node memory management command exists**.
- **A running run holds the absolute old path** in `SPACEMAN_MEMORY_DIR`, in Claude's `--add-dir` and in its system prompt
  (`internal/agent/exec.go:357-437`). A dream run symlinks `memory/<role>` to that role's checkout, and `dream.commit`
  takes the role from the symlink name (`internal/agent/dream.go:55-70, 224`).
- **Durable command queue:** `node_commands` (`idempotency_key` unique per node, re-sent on reconnect, at least once).
  The node journals every command before acting and acks a repeat as `duplicate` (`internal/agent/exec.go:83`). It
  rejects an unknown kind as `unsupported command`. Relocation results are journaled and re-sent on reconnect
  (`resendRelocationReports`), and that is the precedent for a result that needs to reach central.
- Deleting a role deletes no memory, centrally or on the node.

### 1.4 Caches

Central keeps **no cache of space-role configuration**. `Resolve`, gateway grants, pools and messages read the DB on
every use. The only state keyed by role name is:
- the central memory mutex map;
- the node's memory maps (`waiting`, `syncs`, `timers`, locks; `internal/agent/memory.go:42`);
- web clients, which refresh from the change feed.

So "invalidate caches" comes down to three things:
- change-feed records for every rewritten row;
- the memory lock and move protocol;
- dropping the node's in-memory entries for the old key.

### 1.5 Provider

`spaceman_space_role` (`internal/provider/resource_space_role.go`):
- `id = <space_id>/<name>`, using `idAttr` with `UseStateForUnknown`;
- **`name` is `RequiresReplace`**, so a rename today is destroy and create;
- import splits `<space_id>/<name>`.

Role names also appear as strings in other resources:
- `spaceman_mcp_grant.role` (`RequiresReplace`, because grants cannot be edited);
- `spaceman_knowledge_base.space_role` and `allowed_roles`;
- dream `space_role`, `memory_roles` and `transcript_roles`;
- `spaceman_schedule.assignee_role`;
- human permission `agent_roles`;
- human policy `successor_role`.

Other facts:
- The framework is v1.19.0 (it supports `ResourceWithMoveState`), and Terraform must be ≥ 1.11.
- Nothing uses `moved`/MoveState yet.
- The closest precedent is `spaceman_space.node_id`, which moves the space in place through an action API (relocation)
  and is tested to never destroy and recreate (`relocation_test.go`).

---

## 2. Decisions this plan proposes

1. **One action, `POST /spaces/{space_id}/roles/{name}/rename`**, with body `{ "new_name": "…" }`. It renames the row
   and rewrites every **live** reference from §1.2 in **one write transaction**. History stays as recorded.
2. **Refuse while the role is busy.** The rename returns `409 precondition` and lists the blockers when any of these
   exist:
   - a run of the role that is not finished: `desired='running'`, or `observed IN ('pending','starting','running',
     'stopping','unknown')`;
   - an open `limit_waits` or `account_switches` row;
   - a `run.launch` command for the role not yet accepted;
   - a dream run in the space that is not finished, or a pending `dream.commit`;
   - a relocation of the space in progress.

   Allowing the rename while runs are live is rejected, because a live run keeps the old name everywhere it matters.
   Its run token still resolves `runs.role` to the old name, so role messages, MCP grants, KB admission and memory status
   would all miss. Its memory directory and system prompt name the old path. Its node checkpoint timers are keyed by the
   old name. The operator stops or drains those runs first, and the error says which ones.
3. **The target name must be free.** No `space_roles` row may have it, and no `memory_collections` row either (a
   leftover from a deleted role would otherwise merge into this role's memory). Renaming to the same name is a 400.
4. **A rename table, `space_role_renames`,** records `(id, space_id, from_name, to_name, renamed_at, renamed_by,
   node_id, node_state, node_done_at, central_moved_at)`. It serves five purposes:
   - it is the durable intent behind the two memory moves (§3);
   - it resolves an old name to the current one for the relaunch-from-history paths (§1.2) and for dream transcript
     filters;
   - it keeps a renamed-away name from coming back to life through a late memory sync;
   - it lets the provider follow a rename made outside Terraform;
   - it shows the operator the state of the node's move.
5. **Memory moves on both sides, driven by one durable node command** (`memory.rename`, a new protocol minor). Central
   moves its own repository after commit, and recovers that move on startup. Until the node reports the move done,
   launches of the role (and dream launches in that space) are held with a clear reason.
6. **Surfaces:**
   - API: `rename-space-role` and `list-space-role-renames`;
   - CLI: `spaceman role rename <name> --to <new>` and `spaceman role renames`;
   - admin MCP: `rename_space_role` classified **Dangerous** (the same class as `save-space-role`), with the list
     classified Read;
   - Telegram: role ops stay excluded.
7. **Provider:** `name` becomes updatable in place. A changed `name` calls the rename API, and `id` is recomputed. A
   `moved` block is needed only when the user also renames the Terraform address, which Terraform handles without any
   provider support. **Never a replace.**

---

## 3. Design

### 3.1 The transaction (central)

`catalog.Service.RenameSpaceRole(ctx, actor, spaceID, from, to string, ifRevision int64)`, in
`TxNamed("catalog.space_role_rename")`. It does no file or network I/O, and its body is re-runnable because BUSY
retries:

1. Validate `to` against the role regex. Load `space_roles(space, from)`; if it is missing, return 404. If an
   `If-Match` revision is given and differs, return 409 `precondition_failed`. (This departs from the earlier 412 on
   purpose, to match the API's documented status contract; see §3.4.)
2. **Blockers** (decision 2). Collect all of them, then fail once with the list (run ids, wait ids, `relocation`,
   `dream run …`).
3. **Target free** (decision 3).
4. **Rewrite, scoped to the space** (one sqlc query each; every query is `WHERE space_id = ? AND <col> = ?`):
   - `space_roles.name` (bump `revision`/`version`).
   - `mcp_grants.role`.
   - `kb_repos.space_role`; `kb_repos.allowed_roles_json` (replace the element, keep the order, leave `*` alone).
   - `dream_configs.space_role`, `memory_roles_json`, `transcript_roles_json`.
   - `space_schedules.assignee_role`.
   - `human_policies.successor_role`.
   - `space_human_type_permissions.agent_roles_json`.
   - `space_role_accounts.role`.
   - `memory_collections.role`.
   - `tasks.assignee_role` **only for open tasks** (not done, failed or cancelled; bump each task's version).
   - `messages.to_id` where `to_kind='role'` and `state IN ('stored','available')`.
   - `kb_requests.requester_role` for open requests.
   - `human_notifications.target_role` for pending notifications.

   Rewrite JSON arrays in Go: read the row, decode, replace exact matches, de-duplicate, encode with the same encoder
   the service uses, write. Do not use SQL `json_*`, so the arrays stay byte-identical to what a save would produce.
5. **History is not touched:** `runs.*`, `run_usage`, `usage_buckets`, `approvals`, `xspace_requests`,
   `kb_sessions`, `human_actions.requester_role`, `inbox_items`, `audit_events`, `event_outbox`, the payloads of
   finished `node_commands`, and closed tasks, messages and requests.
6. Insert `space_role_renames`:
   - `node_id` is the space's current node.
   - `node_state` is `pending` when the space has a node; otherwise it is `not_needed`.
   - If the space has a node that negotiated a protocol minor below the new one, **refuse** (`409 node_too_old`). The
     node may hold memory for the role that central has not seen yet (collections are created lazily), so there is no
     safe skip.
7. When `node_state = pending`, enqueue `node_commands(kind='memory.rename', idempotency_key='role-rename:<id>')` (§3.3).
8. `changes.Record`:
   - `space` updated (the existing convention for role changes);
   - one `updated` record for each rewritten row that has its own change type (task, schedule, KB repo, dream, MCP
     grant, human policy, human type permission, account pools).

   Write an audit event `space.role_renamed {from, to, rename_id, rewritten: {table: count}}`. No outbox event: role
   changes publish none today, and adding one is a separate catalog decision.
9. `tx.AfterCommit`: the central repository move (§3.2), then a dispatch kick for the node.

**Resolving old names after the rename.** `catalog.CurrentRoleName(q, space, name)` returns `name` when a
`space_roles` row has it. Otherwise it follows `space_role_renames` from `name` (cycle-safe, at most 16 hops) to a
name that exists, or returns `name` unchanged. It is used by:
- the control supervisor's restart and control autostart, before they copy `Role` from a history row;
- `DreamSpec` building, which adds every former name of each listed transcript role, so the node's filter on its
  `runs.role` history still matches the transcripts from before the rename;
- the human-delivery path that labels a target with `requester_role`.

KB answers need no lookup, because open `kb_requests` are rewritten.

**A role that existed before can be reused.** After renaming `a → b`, creating a new role `a` is allowed. A name
that `space_roles` currently has is never resolved through the rename table, and the memory guard (§3.2) accepts `a`
again once `a` is a real role.

**Holding launches while the node move is pending.** In the launch transaction (`fleet.LaunchRun`), when the space has
a `space_role_renames` row with `node_state='pending'`, refuse with `apperr.CodePrecondition`
(`role_rename_pending`) in two cases:
- the launch's role is that row's `to_name`;
- the launch is a dream run in that space.

The queue and the control supervisor already handle a precondition hold, and the message names the node that is
still to report.

### 3.2 Central memory move

- **After commit**, take the central memory mutexes for both `space/from` and `space/to`, in sorted order to avoid a
  deadlock. Then `os.Rename(<space>/<from>.git, <space>/<to>.git)` and set `central_moved_at`.
  - If `<to>.git` already exists (a repository left on disk with no DB row; space and role deletes remove no
    repository), first move it to `<space>/.retained/<to>-<rename_id>.git`. It is never merged and never deleted.
  - If `<from>.git` does not exist, there was no central memory, and the move simply records itself as done.
- **Crash between commit and move:** `memory.Service` gets `ensureMoved(space, role)`. Under that role's mutex, it
  finishes any `space_role_renames` row for the space whose `to_name = role` and whose `central_moved_at IS NULL`. Every
  repository access calls it first: `repo()`, `Apply`, `Report`, `Files`, `File`, `History`, `ExportBundles` and the
  backup bundler. A startup reconciler runs it for every unfinished row before the hub accepts nodes. The move is
  idempotent: old present and new absent means move; old absent and new present means it is done; both absent means
  done.
- **Guard against resurrection.** `Apply` and `Report` for `(space, role)` where the role is a `from_name` in
  `space_role_renames` **and** `space_roles` has no row with that name answer `memory.ack{result:"error", message:
  "role renamed"}`. They create no collection. While the node move is pending this is expected: the node retries with its
  normal backoff, and after its own rename it resends under the new name. After that, the guard keeps a stale node, or
  one restored from an old backup, from creating a second collection under the old name.
- The backup bundler lists collections from the DB under each collection's mutex (the T-H2 implementor checks the
  current code and makes it do so). Otherwise a backup running at the same moment would look for a path that has just
  moved.

### 3.3 Node memory move: protocol, ordering, failure and retry

**Protocol: minor 14, `MinorRoleRename`.**
- Command `memory.rename`: `MemoryRename{RenameID, SpaceID, From, To}`. It is journaled and idempotent like every
  command, and needs a valid lease for the space and generation like every non-stop command.
- Report `memory.renamed`, node to central: `MemoryRenamed{RenameID, SpaceID, Result: ok|deferred|error, Message}`. The
  node journals it (agent migration 0010: `role_renames(rename_id PK, space_id, from, to, state, result,
  message, reported_at)`) and resends it on every reconnect until central acknowledges it with `memory.renamed.ack`,
  following the relocation-report pattern. Central handles a duplicate report idempotently.
- Central creates the command only for a node whose stored `nodes.protocol_minor` is at least the new minor (§3.1 step
  6 refuses otherwise). This follows the kind-level gating relocation uses.

**Node handling, in order:**
1. Journal the command (existing). If the journal already holds a finished result for `RenameID`, resend that result
   and stop.
2. Take the node memory locks for `space/from` and `space/to` in sorted order.
3. **Busy check.** If the node's journal has a non-terminal run with role `from` in the space, or a dream run in the
   space, do not move. Journal `deferred`, report it, and retry the step when that run ends (hook into the existing
   run-end path) and on agent start. Central's blockers make this rare: it covers runs central does not know about,
   or adopted runs.
4. Stop `space/from` activity: cancel the debounce timer and drop `waiting`/`syncs` entries for the key. An in-flight
   `memory.sync` for `from` that central refuses (§3.2) is harmless: its bundle is not applied, and `memory_sync.acked`
   does not move.
5. If `<space>/<to>` exists, move it to `<space>/.retained/rename-<rename_id>-<to>`. It is never merged and never
   deleted. The T-H3 implementor confirms that `collections()` and `validMemoryKey` skip a dot-directory, and makes
   them skip it if not.
6. `os.Rename(<space>/<from>, <space>/<to>)`, which is atomic on the same filesystem. If `from` is missing and `to` is
   present, the step already happened, so continue. If both are missing, there is nothing to move, so continue.
7. In one agent-DB transaction:
   - move the `memory_sync` row from `from` to `to`; drop any existing `to` row first, since it belonged to the retained
     directory;
   - reset `retries` and `last_error`;
   - mark the journal row `done`.
8. Release the locks. Report `memory.renamed{ok}`, then schedule a sync of `space/to` right away and send `memory.status`.

**Why nothing is lost or duplicated:**
- Git history is never rewritten or copied. One directory is renamed, and `memory_sync.acked` keeps pointing at
  commits central already has. After the rename, the node bundles `acked..main` under the new name, and central applies
  it as a fast-forward of the renamed collection, whose head is unchanged.
- Central never creates a collection for the old name (the guard), and never merges into an existing one (target-free
  check, `.retained`).
- The command is durable on both sides (`node_commands` and the node journal) and idempotent in every step. A crash
  between steps 6 and 7 is repaired by step 6's "already happened" branch when the journaled command is retried at
  start.

**When the node is offline:**
- The rename commits centrally at once.
- The central repository moves at once.
- `memory.rename` stays `pending` in `node_commands`.
- Launches of the renamed role and dream launches in that space are refused with `role_rename_pending` (§3.1).
- `list-space-role-renames` shows `node_state: pending` and the node.
- When the node reconnects, the dispatcher sends the command, the node moves the folder and reports, central marks the
  rename `done`, and the hold is lifted. Nothing the node wrote while offline is lost: it was written under `from`,
  which is the directory being renamed.

**Relocation:** `start-relocation` is refused while a rename with `node_state='pending'` exists for the space (and the
rename is refused during a relocation, decision 2). A node that is gone for good is the open question Q3.

### 3.4 API, CLI, admin MCP

- `rename-space-role`: `POST /spaces/{space_id}/roles/{name}/rename`.
  - Body `{new_name}`, optional `If-Match` (role revision).
  - Returns `200` with the renamed `SpaceRole` plus `rename: {id, node_state, node_id}`.
  - Rule: `InSpace(authz.SpaceConfigWrite)`, the same as save and delete; no step-up, matching delete, which loses more.
  - Errors: 404 (no role), 409 (`role_busy` with the blockers; `name_taken`; `node_too_old`), 409 `precondition_failed`
    (revision), 422 `validation_failed` (invalid or same name). This departs from the earlier 412 and 400 on purpose, to
    match the API's documented status contract (`site/reference/api.md`).
- `list-space-role-renames`: `GET /spaces/{space_id}/role-renames`, newest first, read rule `SpaceConfigRead`. The
  provider uses it to follow a rename made outside Terraform (§3.5), and the operator uses it to see a pending node
  move.
- `get-space-role` on a renamed-away name stays 404, and its message adds `renamed to <to>` when the name resolves.
- CLI (ADR-0040, bound by op id), in `internal/cli/cmd_config.go`:
  - `spaceman role rename <name> --to <new>` (done message "Role renamed.", plus "memory moves when node X reconnects"
    when `pending`);
  - `spaceman role renames`;
  - `site/reference/cli/role.md` is regenerated with `make cli-docs`.
- Admin MCP: `classify.go` Table entries are `rename-space-role: {Dangerous, rSpaceRole}` and
  `list-space-role-renames: {Read, …}`. Telegram: add both to `api.TelegramExcluded` with the role-ops reason.
- `api/openapi.json` and `web/src/api/schema.gen.ts` are regenerated (`make api`).
- Docs:
  - `site/guide/roles-and-prompts.md` gets a new section, "Rename a role", covering what moves, what stays as history,
    the busy refusal and the pending node move;
  - add a line to the memory page;
  - `CHANGELOG.md` gets Added, plus an Upgrade note: the rename needs nodes on the new protocol minor.

### 3.5 Provider (terraform-provider-spaceman)

- `spaceman_space_role`:
  - Drop `RequiresReplace` from `name`.
  - Replace `id`'s `UseStateForUnknown` with a modifier that keeps the state value unless `name` changes, in which case
    it marks the value unknown.
  - `space_id` stays `RequiresReplace`.
- `Update`: if the planned name differs from the state name, call `RenameSpaceRole(space, old, new)` first, then the
  existing `SaveSpaceRole` PUT with the new name for any other changed attribute, then read back. If the rename returns
  409, surface the blockers as the diagnostic. Never fall back to delete and create.
- `Read`: on 404 for the state name, call `list-space-role-renames` and follow from the state name to a role that
  exists. If one exists, set `name`/`id` to it, so a rename made outside Terraform shows as an in-place `name` change
  back to the configured value, not as a resource to create. A 404 with no rename still removes the resource from
  state (current behaviour).
- `ImportState` is unchanged (`<space_id>/<name>`).
- **Dependents**, which reference `spaceman_space_role.x.name` or a literal:
  - kb, dream, schedule, human permission and human policy are plain in-place updates. Central has already rewritten
    them, so their PUT is idempotent.
  - `spaceman_mcp_grant.role` stays `RequiresReplace`, because grants cannot be edited. Terraform revokes the grant by
    id and adds one for the new role. That is safe because a rename requires no live runs of the role, so no run sees
    the revocation. The docs say this. Making grant role edits possible is out of scope.
- `moved {}`: only needed when the user also renames the resource address (`spaceman_space_role.implementor` →
  `.implementor_hard`). Terraform core handles that for the same resource type, so no `MoveState` is needed. The
  resource docs show the example.
- The provider needs the SpaceMan version that adds the rename. On an older server, `Update` with a name change fails
  with "renaming a role needs SpaceMan ≥ <version>" (404 on the action path).
- `docs/resources/space_role.md` is regenerated (`SM_TF_DOCS=1 go test ./internal/provider -run TestDocsUpToDate`).
  The CHANGELOG gets an entry, and the resource count is unchanged.

---

## 4. Failure modes

| Case | Behaviour |
|---|---|
| Live run, open limit wait or switch, queued launch, dream, relocation | 409 listing the blockers; nothing changes |
| Target name exists as a role or a memory collection | 409 `name_taken` |
| Space's node is below the new minor | 409 `node_too_old` (upgrade the node first) |
| Central crashes after commit, before the repo move | `ensureMoved` on the next access, and the startup reconciler |
| Node offline | rename committed; node command pending; launches of the role and dreams in the space held; done on reconnect |
| Node busy (run the central side did not know about) | `deferred` reported and shown; retried at run end and at start |
| Old node, or one restored from backup, syncs the old name | `memory.ack error "role renamed"`; no collection created |
| Leftover `<to>` directory or repo | moved to `.retained/…`, never merged or deleted |
| Rename chain `a→b→c`, then a new `a` | `CurrentRoleName` follows the chain only for names without a live row |
| BUSY retry of the transaction | body re-runnable; no I/O inside |

---

## 5. Tasks

Tiers: **hard** = `implementor-hard`, **medium** = `implementor-medium`, **light** = `implementor-light`. Each task is
one branch and one draft PR into `next/v0.1.9`. The provider task is a PR into the provider repository.

```
T-M0 ──► T-H1 ──► T-H2 ──► T-H3
             │        │        └──► T-L3
             │        └──► T-M1 ──► T-L2
             │                 └──► T-M2 (provider)
             └──► T-L1
```

### T-M0: ADR, requirements and docs skeleton (medium, docs only)
- **Goal:** land §7 in `docs/DECISIONS.md` as ADR-0093, and add SM-PROMPT-009 (rename) and SM-MEM-007 (memory move)
  to `docs/GOAL.md` and `docs/TRACEABILITY.md` (status *planned*). Record the two follow-ups (Q3 abandon the node move,
  Q5 web UI rename) and the Q4 limitation in `docs/LIMITATIONS.md`.
- **Files:** `docs/DECISIONS.md`, `docs/GOAL.md`, `docs/TRACEABILITY.md`, `docs/LIMITATIONS.md`, `docs/PLAN.md`; this
  plan (link the ADR).
- **Acceptance:** `make site-check` passes; no implementation status is claimed.
- **Depends on:** PR #195 merged (it takes ADR-0092).

### T-H1: transactional rename and name resolution (hard)
- **Goal:** §3.1 without the node command or the central repository move. Write `node_state` already, but leave
  `central_moved_at` NULL and enqueue nothing.
- **Files:**
  - central migration 0061 (`space_role_renames`) and `internal/store/queries/catalog.sql` (rewrite queries);
  - `internal/catalog/config.go` (`RenameSpaceRole`, `CurrentRoleName`);
  - the callers in `internal/control/supervisor.go`, `internal/control/lifecycle.go`, `internal/dream/dream.go` and
    `internal/human/deliver.go`;
  - the launch hold in `internal/fleet/runs.go`.
- **Acceptance:**
  1. A table-driven test seeds **every** row type in §1.2 for the role (and the same rows for another role and another
     space), renames, and asserts:
     - every live row has the new name;
     - every history row, closed task, acknowledged message, other role and other space is unchanged.
  2. Each blocker in decision 2 gives a 409 that names it, and changes nothing.
  3. JSON arrays keep their order, leave `*` alone and remove a duplicate.
  4. The test runs the body twice (BUSY retry) and the result is the same.
  5. Control restart and autostart after a rename launch under the new name.
  6. A launch of `to` while `node_state='pending'` is refused with `role_rename_pending`.
  7. Audit and change-feed records are written.
  8. `make check-fast` passes.
- **Risks:**
  - A column added later and missed. T-L1's schema guard covers it.
  - `usage_buckets` must not be rewritten (PK collision).

### T-H2: central memory move, guard and protocol (hard)
- **Goal:** §3.2, plus the central half of §3.3:
  - proto types `MemoryRename`, `MemoryRenamed`, `MemoryRenamedAck` and the new minor, with a changelog comment in
    `proto.go`;
  - enqueue the command in T-H1's transaction;
  - refuse `node_too_old`;
  - handle `memory.renamed` in the hub (idempotent);
  - the hold is lifted on `ok`;
  - relocation refused while a rename is pending.
- **Files:** `internal/memory/memory.go`, `internal/backup/backup.go`, `internal/proto/`, `internal/fleet/hub.go`,
  `internal/fleet/relocation.go`, `internal/catalog/config.go`.
- **Acceptance:**
  1. The repository is renamed after commit.
  2. A simulated crash (commit without the after-commit hook) is fixed by `ensureMoved` on the next `Files`/`Apply`, and
     by the startup reconciler.
  3. A leftover `<to>.git` is moved to `.retained`.
  4. `Apply`/`Report` for the old name return `error "role renamed"` and create no row, unless `space_roles` has that
     name again.
  5. A sync under the new name fast-forwards the existing head.
  6. A duplicate `memory.renamed` is a no-op.
  7. A backup taken during a rename succeeds.
  8. Proto round-trip tests pass.
  9. `make check-fast` passes.
- **Depends on:** T-H1 (and PR #195, which changes `internal/memory/memory.go`).
- **Risks:**
  - Lock ordering with `Apply`'s per-collection mutex (sort the keys).
  - The backup bundler's path listing.
- **Done (2026-11-16, `feat/role-rename-memory`).** Deviations are in ADR-0093's T-H2 implementation notes: migration
  0062 adds the `failed` node state; the hub sends the command at the space's current lease generation and re-queues
  one the node refused; the guard also refuses a reused old name while the node move is open; a backup holds the moves
  from its snapshot until it has bundled the memory (the bundler walks the disk, so the plan's "list from the DB" was
  replaced by this hold).

### T-H3: node-side memory rename (hard)
- **Goal:** the node half of §3.3:
  - the command handler;
  - the agent migration (journal of rename results) and a resend on reconnect;
  - the busy deferral with retry at run end and at start;
  - the `.retained` handling;
  - dropping the in-memory entries;
  - the immediate resync.
- **Files:** `internal/agent/exec.go`, `internal/agent/memory.go`, `internal/agent/journal.go`,
  `internal/agent/agent.go`, `internal/agent/migrations/`.
- **Acceptance:** agent tests for:
  - rename with no run;
  - rename while a run of the role is active (`deferred`, then done after the run ends);
  - repeat command (`duplicate`, same result resent);
  - crash after `os.Rename`, before the DB update (resumes to done);
  - both directories missing (ok);
  - `<to>` present (retained, not merged);
  - `memory_sync` row moved and backoff reset;
  - the next sync bundles from the old `acked`.

  Instance checkouts still fetch and publish after the move. `make check-fast` passes.
- **Depends on:** T-H2 (proto types).
- **What T-H2 leaves for it:**
  - raise `agentMaxMinor` (`internal/agent/agent.go`) to `proto.MaxMinor` once the handler exists;
  - accept `memory.rename` with the lease at the command's generation (central sends the current one);
  - answer `error` only for a move that failed, keep it journaled, retry it at the next agent start and report again
    (central turns a failed rename done on a later `ok`, and pending on `deferred`);
  - resend every unacknowledged `memory.renamed` on reconnect until `memory.renamed.ack` arrives; an ack with `error`
    (unknown rename, or not this node's) also ends the resend;
  - after the move, sync under the new name: central refuses the old name with `memory.ack{error, "role renamed"}`.
- **Risks:**
  - The dream symlink layout. Central's dream blocker and the node busy check both cover it; add a test.
  - Lease checks for the command.

### T-M1: API, CLI, admin MCP, docs (medium)
- **Goal:** §3.4.
- **Files:**
  - `internal/api/catalog.go` and the catalog rules map;
  - `api/openapi.json` and `web/src/api/schema.gen.ts` (`make api`);
  - `internal/cli/cmd_config.go` and the generated CLI docs;
  - `internal/spacemanmcp/classify.go`;
  - `internal/telegram/ops.go`;
  - `site/guide/roles-and-prompts.md` and the memory guide page;
  - `CHANGELOG.md`.
- **Acceptance:**
  - `TestEveryOperationHasARule`, `TestEveryOperationHasACommand`, `TestEveryOperationIsClassified` and
    `TestTelegramClassificationIsExhaustive` pass;
  - `make api-check`, `make site-check` and `make check-fast` pass;
  - the error codes in §3.4 are mapped to the HTTP statuses listed there.
- **Depends on:** T-H1. The `rename.node_state` field is real once T-H2 lands; before that it reads from T-H1's column.

### T-M2: provider in-place rename (medium; terraform-provider-spaceman)
- **Goal:** §3.5.
- **Files:**
  - `internal/client/api.go` (`RenameSpaceRole`, `ListSpaceRoleRenames`);
  - `internal/provider/resource_space_role.go`;
  - `resource_mcp.go` (docs only);
  - `docs/resources/space_role.md` and the example with `moved`;
  - `CHANGELOG.md`.
- **Acceptance:**
  1. A unit plan test shows that changing `name` plans an **update** (no replace) with `id` unknown.
  2. An acceptance test (`TF_ACC`, with `SPACEMAN_SRC` pointing at a checkout containing T-M1) runs:
     1. create a role plus a schedule, KB, dream, human permission, human policy and MCP grant that reference it by
        `spaceman_space_role.x.name`;
     2. rename the role in one apply;
     3. check that the role's ID changed but it was never destroyed (a check on `revision` continuity);
     4. check that the second plan is empty;
     5. rename the role outside Terraform through the API; the next plan shows an in-place `name` update, and applying
        it renames the role back;
     6. a `moved` block that also renames the address plans no destroy.
  3. The docs are regenerated.
- **Depends on:** T-M1 (and a SpaceMan release containing it before the provider release).

### T-L1: schema guard for role-name columns (light)
- **Goal:** a test that lists every central table column whose name matches `role`, `assignee_role`,
  `requester_role`, `successor_role`, `target_role`, `*_roles_json` or `space_role` (from `PRAGMA table_info` after
  migrations). It fails when one is not in an explicit `live`/`history`/`not-a-space-role` classification next to
  `RenameSpaceRole`. It follows the pattern of the existing exhaustive classification tests.
- **Acceptance:**
  - the test passes on the T-H1 tree;
  - adding a dummy `foo_role` column in a test migration makes it fail with a message pointing at the classification;
  - `make check-fast` passes.
- **Depends on:** T-H1.

### T-L2: API and CLI tests (light)
- **Goal:** API e2e tests in the `internal/app` style:
  - 200, 404, 422 (same name), 409 `role_busy` (a running run of the role), 409 `name_taken`, 409
    `precondition_failed` (stale If-Match). These depart from the earlier 400 and 412 on purpose, to match the API's
    documented status contract (§3.4);
  - viewer denied, operator allowed;
  - `list-space-role-renames` order.

  CLI golden tests for `role rename` and `role renames`.
- **Acceptance:** the new tests pass; `make check-fast` passes.
- **Depends on:** T-M1.

### T-L3: end-to-end memory move with an offline node (light)
- **Goal:** an `internal/app` e2e with the existing fake or real in-process node:
  1. write memory for role `a` and sync it;
  2. disconnect the node;
  3. rename `a → b`;
  4. check that a launch of `b` is held and the renames list shows `pending`;
  5. write more memory under `a` on the node while it is disconnected (offline);
  6. reconnect.

  Then assert:
  - the node directory is `b`;
  - central has exactly one collection `b`, whose head contains both commits;
  - there is no collection `a`;
  - the hold is lifted;
  - the rename is `done`.

  A second case renames with the node online.
- **Acceptance:** the tests pass under `-race` (`go test -race ./internal/app -run …`).
- **Depends on:** T-H3.

**Closing (control):** once the PRs are merged, run `make check` on `next/v0.1.9`, update `PLAN.md`/`TRACEABILITY.md`
evidence (status *implemented, tested (simulated)*), and tell the requester the operation exists, with the CLI and
Terraform usage.

---

## 6. Evidence and tests summary

- Simulated: every test above (fake node or in-process node, temp directories). No real external dependency is
  involved. Everything is local Git and SQLite.
- Not verified until it runs on a real node: a rename against a node holding a large real memory history. The design
  does not depend on size, since it is one `rename(2)`.

---

## 7. ADR draft

### ADR-0093 — A space role can be renamed in place; live references follow, history stays

- **Status:** Proposed, 2026-11-16. Central migration 0061 (`space_role_renames`), agent migration 0010 (rename results),
  protocol minor 14 (`memory.rename`, `memory.renamed`). Requirements SM-PROMPT-009 and SM-MEM-007. New API ops `rename-space-role` and `list-space-role-renames`.
- **Context:** a space role's name is its identity: the primary key of `space_roles`. It is also copied as text into
  grants, pools, KB and dream configuration, schedules, human policies and permissions, open tasks, messages and
  requests, memory collections (including the directory names of the central and node Git repositories) and the history
  of every run. No foreign key ties these copies to the role. Deleting and recreating a role under a new name keeps the
  stale copies: grants, pools and routes silently stop matching, and the role's memory stays under the old name. The
  Terraform provider replaces the role on a name change, which does exactly that.
- **Decision:**
  1. A rename is one write transaction. It changes the role row and every **live** reference: configuration, plus open
     tasks, pending messages, open KB requests and pending notifications. It records a `space_role_renames` row. Records
     of what happened keep the name they were written with: runs and their manifests, usage, approvals, cross-space and
     KB requester history, human actions, inbox items, audit and outbox.
  2. The rename is refused while the role is busy: an unfinished run, an open limit wait or account switch, a queued
     launch, an unfinished dream run in the space, or a relocation. A live run carries the name in its token,
     environment, prompt and memory path, so renaming under it would split its routes and memory. The error lists what
     to stop.
  3. The target name must be unused by a role and by a memory collection. Existing memory is never merged.
  4. Memory moves on both sides without copying or rewriting history:
     - central renames its repository after commit, and finishes an interrupted move on the next access or at start;
     - the space's node renames its role directory on a durable, idempotent `memory.rename` command and reports
       `memory.renamed`;
     - until the node has reported, launches of the role and dream runs in the space are held;
     - central never creates a collection under a name that was renamed away and has not been reused;
     - a node below the new protocol minor makes the rename fail rather than risk unsynced memory.
  5. Old names resolve to the current name only where SpaceMan relaunches from a history row (control restart and
     autostart) and for the dream transcript filter. A name that is a live role is never redirected.
  6. The rename is exposed as `POST /spaces/{space_id}/roles/{name}/rename` with the same permission as saving a role.
     It is also in the CLI (`spaceman role rename`) and the admin MCP (classified dangerous). The Terraform provider
     treats a name change as an in-place update that calls it, never as a replace.
- **Alternatives:**
  - *Allow renaming a live role and update the runs:* every live run's token, environment, system prompt, memory path
    and node timers would have to change under a running agent. Rejected.
  - *Key memory repositories by collection id instead of role name:* this removes the directory moves but needs a disk
    layout migration on central, every node and in backups. Rejected as larger and riskier than one atomic rename per
    side.
  - *`ON UPDATE CASCADE` foreign keys:* half the references are JSON arrays or conditional (open rows only), and SQLite
    would need table rebuilds. Rejected.
  - *Terraform `MoveState`:* only needed across resource types. A name change is an update, and renaming the address is
    covered by core `moved`.
- **Consequences:**
  - Renaming needs a quiet role.
  - With the node offline, the role cannot launch until the node reconnects.
  - Former names remain in history and in the rename table.
  - An MCP grant on the role is revoked and re-added by Terraform, because grants cannot be edited.
  - A leftover directory under the target name is moved aside to `.retained`, never deleted.
  - Deleting a role still leaves its memory collection, and that blocks reusing the name as a rename target: pick
    another name. Deleting or archiving a role's memory is a follow-up.
  - A node that never comes back keeps the renamed role held. There is no "abandon the node move" action yet; it is a
    follow-up.
  - A test classifies every role-name column as live or history, so a new column cannot be missed.
- **Not security-relevant:** no requirement, guarantee or default is relaxed. The operation uses the existing role
  write permission, and every rename is audited.

---

## 8. Decisions (2026-11-16)

Control decided every question on 2026-11-16.

- **Q0 (numbers):**
  - **ADR-0093** and central migration **0061**: PR #195 takes ADR-0092 and migration 0060.
  - Agent migration **0010**, protocol minor **14**, and requirements **SM-PROMPT-009** and **SM-MEM-007**. Protocol
    minor 14 and both requirement IDs were checked as free on 2026-11-16: no branch has a `MaxMinor` above 13 and no
    branch uses either requirement ID. PR #195 adds no protocol minor; it adds only a status field.
  - **Agent migration number changed from 0009 to 0010.** PR #195 merged first and took agent migration 0009
    (`0009_memory_conflict_folders`), so the rename's journal is 0010. Central migration 0061, ADR-0093 and minor 14
    were re-checked as free on `next/v0.1.9` at 9598b1af.
- **Q1:** a busy role is refused, and the error lists the blockers (decision 2).
- **Q2:** no step-up beyond the existing permission to write a space's configuration (`SpaceConfigWrite`, as for saving
  and deleting a role). The admin MCP tool stays classified Dangerous.
- **Q3:** no "abandon the node move" action in this version. It is recorded as a follow-up (T-M0, `LIMITATIONS.md`).
- **Q4:** accepted for now. A deleted role's leftover memory collection blocks reusing its name as a rename target; the
  ADR's consequences say so. Deleting or archiving a role's memory is a follow-up.
- **Q5:** the web UI rename action is a follow-up and is not part of these tasks.
