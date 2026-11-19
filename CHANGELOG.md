# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Dream runs can now write the role memories they link.** The `memory/<role>` links in a dream's workspace point
  at memory folders outside it, which Claude Code refused to write through. The run now gets write access to exactly
  those folders of its own space, and nothing else.

## [0.1.14] - 2026-11-19

### Added

- **`harness.env_passthrough` in the agent configuration** names extra environment variables the agent copies from
  its own environment into Claude Code and Codex runs (default: none), for example `CLAUDE_CODE_TMPDIR`. Only exact
  names are accepted; patterns and empty entries are refused. These are refused: the variables the agent sets or
  reserves itself (`SPACEMAN_*`, `HC_*`, `HALLPASS_*`, `XDG_*`, `CLAUDE_CONFIG_*`, `HOME`, `PATH`, `TERM`, `COLORTERM`,
  `PWD`, `USER`, `LOGNAME`, `SHELL`, `CLAUDECODE`, `CLAUDE_PID`, `CLAUDE_EFFORT`, `DISABLE_AUTOUPDATER`,
  `DISABLE_UPDATES`); the agent's own and the harness's credential variables (`ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_TOKEN`,
  `HARBOR_PILOT_TOKEN`, and every `CLAUDE_CODE_*` outside a short safe list); model-provider credentials and endpoints
  (`AWS_*`, `GOOGLE_*`, `CLOUD_ML_*`, `VERTEX_*`, `AZURE_*`, `OPENAI_*`, `CODEX_*`); proxy and TLS variables
  (`HTTPS_PROXY`, `NODE_OPTIONS`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, …); and loader hooks (`LD_*`, `BASH_ENV`, `ENV`).
  The agent's own value always wins. The operator remains responsible for any other names
  they list. A profile's `claude_settings.env` stays the per-profile way to set Claude Code variables.

- **The watchdog reminds workers that forget to hand back their task.** A live worker whose turn ended while its task
  is still `running`, and that neither set the task to review or blocked nor messaged control, gets one reminder in
  its terminal after `worker_idle_watchdog_minutes` (default 10; 0 turns it off). If the reminder's turn leaves the task
  running, control gets one notification naming the run and the task as long again later. A worker waiting for a
  person, at a usage limit or taken over is left alone; one whose message to control is still open is waiting for
  control, so control is told instead. Each reminder is audited (`worker.watchdog_nudged`).

- **The watchdog flags worker runs whose program never connected to SpaceMan.** A running worker run whose program
  has not loaded its SpaceMan tools (the MCP bridge fetches them when the program starts) and made no SpaceMan tool
  call `worker_start_watchdog_minutes` (default 10; 0 turns it off) after it started gets control one notification
  naming the run and its task, once per run. It asks control to check the run's terminal rather than stop it. A long
  first turn, a run that waited at a usage limit and a run that started before central's last restart are never
  flagged. `spaceman_runs_list` shows each run's `turn`.

- **A sweep removes leftover recordings of deleted runs.** A recording folder left on the local backend, and S3
  objects of a run with no spool folder, were never removed after the run's recording was deleted or the run was
  gone. Central now sweeps the recordings directory and the bucket at start and every
  `artifacts.recording_reconcile_interval` (default 24h, `0s` turns it off). It removes only recordings of runs with
  `recording_deleted_at` set, plus, with `artifacts.recording_reconcile_absent: true` (off by default, because a
  restored older backup or a second central on the same bucket also looks like "absent"), runs the database confirms
  absent whose recording has been untouched for 24h. A database or listing error skips the run, a run that exists is
  never touched, and each pass is bounded (200 runs, continuing where the last stopped) and paced.

- **A gateway run's profile may set `CLAUDE_CODE_TMPDIR`.** It moves Claude Code's temp directory off `/tmp`, so a
  profile's `claude_settings.env` can use it. It is on the gateway allowlist of output and UI switches: it carries no
  credential or endpoint, and every other `CLAUDE_CODE_*` variable is still refused.

### Changed

- **The recording sweep finds leftovers with one query.** Instead of a database lookup per stored run, central asks
  once per batch of 500 stored run IDs which of them have `recording_deleted_at` set (by primary key, so no new
  index or migration is needed), so all leftovers are found in one pass and removed within ceil(leftovers/200)
  passes, instead of up to ceil(N/200) passes for N stored runs. The 200 limit now bounds removals per pass. A
  database error ends the pass with nothing deleted, and the opt-in
  `recording_reconcile_absent` mode still uses the per-run check unchanged.
- **The recording sweep lists one folder per run on S3.** With `artifacts.recording_reconcile_absent` off, the sweep
  no longer lists every segment object (memory grew with history); it lists the run folders with a `/` delimiter.
  Object ages are still read when absent mode is on. The reference now says that a removal also sets central's
  in-memory deleted mark for the run, and that with absent mode on a leftover can wait up to ceil(N/200) sweeps when
  N runs are stored.
  A space purge whose recording delete fails still leaves a leftover only the opt-in absent mode removes; this is
  documented in the code.

### Fixed

- **Pausing, disabling or reconfiguring an event subscription no longer delays an event by 30 seconds.** The ingestion
  manager stopped a durable subscription immediately, so an event the server delivered during the stop stayed
  unacknowledged, and the consumer started next (after the space resumed, or with the new configuration) received it
  only once the 30-second acknowledgement timeout expired. Subscriptions are now drained: events already delivered are
  handled and acknowledged first, for at most 5 seconds. The go-pulse SDK is updated to v0.1.7 for this
  (it also moves go-jose from 4.1.4 to 4.1.6). Events already delivered when a `while_paused=stop` space pauses are
  ingested during the drain. If the drain times out, the events still buffered no longer set the subscription back to
  `running` or count as rate limited.

- **Messages and notifications now reach a control (or worker) that waits at its prompt while background tasks run.**
  After a turn, Claude Code reports `working` ("1 shell still running") instead of `done` while background shells or
  monitors it started are still running. SpaceMan took that for a turn in progress, so no terminal notice
  was typed until the background work ended or the agent read its messages on its own. Workers' messages and
  task-review notifications to control could wait for most of an hour. The node now reports such a run with the
  status kind `background`. It takes idle-gated notices like a `done` run and is noticed as soon as it starts
  waiting, while control's idle stop, account switches and the watchdog still treat it as working (ADR-0100).

- **Turning the worker start check back on no longer flags long-running runs.** While `worker_start_watchdog_minutes`
  is 0 the watchdog now keeps the start records of running runs, so a run that began long ago is not reported as
  never started after the check is re-enabled.

- **A close record numbered while the local disk failed is no longer sent twice.** The close reached central but not the
  disk, so a later recorder for the run (started from a stale snapshot) read an open recording and ended the run again.
  The agent now remembers, per run and in memory, that it numbered the close, and a later recorder does not add another. The mark does not survive an agent restart, so a close that is on central but
  not on disk, followed by a restart, can still be sent twice; closing that case needs central to report its last record.
- **The agent's catch-up pass and capability reports are joined at shutdown.** `Run` started the recording catch-up
  pass, the capability loop and the report after hello as untracked goroutines, so one could still be running after
  `Run` returned (and, in tests, read package settings the caller restored next, a data race). They are tracked now and
  `Run` waits for them, bounded, like its other background work.
- **A capabilities report is written only on the connection it was checked against.** The agent now reads what it has
  sent, the protocol and the connection in one step and sends on that exact connection, so a reconnect in between makes
  the write fail instead of putting an older inventory on the new session.
- **A launch that fails before its program starts ends the run under the observe lock too.** Its failed write now takes
  the observe lock after the run's lock, like a stop, and the end work runs after the locks are released. A stop for a
  run this node had no record of takes it too, so every write that ends a run takes the observe lock.
- **An older capabilities inventory no longer overwrites a newer one at central.** The agent now remembers the newest
  capabilities generation it sent (a hello counts) and skips any report at or below it, with the check and the send
  as one step. Before, the report sent after a hello could read the cache, lose a race with the periodic/change
  report of a newer generation, and land last, leaving central stale until the next change or the 30-minute report.
  It also drops the duplicate identical reports.
- **Live terminal output is no longer held through the recording catch-up's backoff.** While central left one run
  unanswered the catch-up pass slept (1 s up to 30 s) with every run marked not caught up by a new request, so live
  output waited for the sleep to end. A new request or the first answer for a run now wakes it.
- **A stop can no longer be overwritten by a launch** (#272 and follow-ups). The managed Claude Code launch's `starting`
  write, and now also a pre-start failure (download, build, credentials, secrets), check that the run was not stopped
  and write under one lock, so a stop landing in between is kept. A launch re-delivered after a crash between starting
  the program and journaling it re-checks the run under that lock before adopting the program: when the run was stopped
  meanwhile, it stops the found program instead of recording it as running.
- **A run stopped before it had a program now gets its end-of-run work** (follow-up to #273). The run-end work (forget
  hook data, memory flush, final usage, deferred role renames, retiring the idle lease guard, removing saved
  attachments) is one helper. It now also runs when a stop ends a run before start (the guard the launch may have
  started is retired) and when a launch adopts a program of a run already stopped, which no longer leaks an idle guard
  or the run's attachments. The wait for that program to go after its kill has tests, and `TestGatewayRevokedOnPurge`
  no longer runs in parallel with the package's other tests, where it could pass the 500 ms write-lock limit.
- **The run-end work runs exactly once, and an abandoned launch retires its guard** (follow-up to #276). A launch that
  is abandoned after it started the space's lease guard retires it (a stop may have retired the guard before it was
  started). The runner reconcile re-checks a pending or lost run under the run's lock before it ends it, so a
  re-delivered launch or an observe is not overwritten and the end work does not run twice. Reading a run's previous row
  and writing the new one in `observe` is one locked step, so two concurrent observes (the monitor and a stop) cannot
  both see the run end.
- **A deleted recording stays deleted** (#262, #263, #264). Late chunks that arrive after the recordings were closed, or
  for a deleted recording, are dropped instead of recreating it; the deleted state is durable across restarts and
  against an S3 archive racing a delete; and central stops serving a deleted recording (`410 Gone`) and cleans up
  its leftovers at startup.
- **The terminal treats a deleted recording as final** (#266). The web terminal handles a `410` for a recording as the
  end of that recording, not as an error to retry.
- **A recording ends with its close record when the program ended before the recorder attached.** A program that
  exits between the agent adopting it (after a restart) and its recorder attaching, or between two attachments, can no
  longer be attached; the recorder used to stop without a record, so neither the local recording nor central's copy
  said the program had ended. It now records a gap (`finished`) and the program's close record, numbered above
  central's sequence like any other. A second recorder started for a run whose close is already recorded writes
  nothing more.
- **Test flakes** (#262, #267). The recording-retention test and the control watchdog test wait for the event they
  check instead of passing early.

## [0.1.13] - 2026-11-18

The `v0.1.11` and `v0.1.12` tags were created but never released (their release runs stopped at the gates, so no
images, archives or releases exist for them). This release ships everything planned for 0.1.11, plus the fix below for
a run stopped before its launch reached the node. The Terraform provider 0.1.11 works with it.

### Upgrade notes

- **Upgrade central first, then the node agents.** v0.1.13 speaks node protocol **minor 14** (v0.1.10: 13). Older
  node agents keep working but miss the node-side fixes (memory guard, harness-config filter, fast-forward, recording)
  and cannot take a role rename (see the last note).
- **Schema migrations 0061–0065** are applied on start, need no action and are forward-only: `0061_space_role_renames`,
  `0062_role_rename_failed`, `0063_role_rename_abandoned` (role rename, ADR-0093), `0064_limit_wait_cause` (a read-only
  `cause` on limit waits, ADR-0098) and `0065_space_role_description` (a space role's own description, ADR-0095).
  Take a backup before upgrading, as always. The node agent's journal gets migration `0010_role_renames`.
- **New settings, all optional.** Three space limits, `control_watchdog_minutes` (default 15, 0 turns the watchdog
  off), `control_watchdog_nudges` (default 3) and `control_watchdog_restart` (default off); and the node agent key
  `memory.fast_forward` (default on). The watchdog is therefore **on by default** in every space with a control.
  See the control guide.
- **Memory checkouts are stricter.** The node agent now refuses to run git in a memory checkout whose `.git` is not a
  real directory of its own, whose git configuration redirects or hooks git, or that holds links inside `.git`, a nested
  repository or a FIFO. The first time the node guards an existing checkout after the upgrade (its next run start or
  checkpoint), it removes the harness files git tracks there from the working tree; harness files written later stay in
  that checkout, unshared. Normal checkouts need nothing. A checkout the guard refuses (for example `.git` as a file or
  symlink, `objects/info/alternates`, a nested `.git` or a FIFO in the work tree, a gitlink, or `.git/config` setting
  `core.worktree`, hooks or a non-boolean `pager.*`) is not repaired: its runs start without memory and the node logs
  `role memory unavailable for this run`. Fix what the reason names, or delete that instance's checkout under
  `<data dir>/memory/<space>/<role>/instances/<instance>` (unpublished changes in it are lost, the shared history is
  kept). See troubleshooting, "Memory checkout refused". It never stages or materialises harness configuration
  (`.claude/`, `.codex/`, `.agents/`, a top-level `.mcp.json`) from role memory. Memory notes stay as they
  are; only such files stop travelling.
- **Renaming a role needs nodes on node protocol minor 14.** The rename moves the role's memory folder on the space's
  node (`memory.rename`), so a space whose node last connected with an older agent, or never connected, is refused
  (`409 node_too_old`) until the node agent is upgraded and has connected. Upgrade central first.

### Added

- **Rename a space role in place** (ADR-0093). `POST /spaces/{space_id}/roles/{name}/rename`,
  `spaceman role rename NAME --to NEW` and the administrator MCP tool `rename_space_role` (classified dangerous) rename a role and every
  live reference to it in one transaction: MCP grants, knowledge-base and dream configuration, schedules, the human policy
  successor, human type permissions, the account pool, memory, open tasks, unacknowledged messages, open knowledge-base
  requests and pending notifications. History keeps the name it was written with. A busy role is refused (`409 role_busy`,
  listing what to stop), as is a name already used by a role or a memory collection (`409 name_taken`); an invalid name is
  `422 validation_failed` and a stale `If-Match` is `409 precondition_failed`. `GET /spaces/{space_id}/role-renames` and `spaceman role renames` list the renames and the state of each
  memory move. Telegram does not offer them. See the roles guide.
  The role's memory moves with it: central renames its repository right after the commit (after a backup that is
  bundling memory, if one is) and finishes an interrupted move on the next access or at start, and the space's node gets
  a durable `memory.rename` command (node protocol minor 14) and reports `memory.renamed`. The rename's `node_state` is
  `pending` until then, `failed` with the node's reason if it could not move the folder, and `done` once it has; until it
  is done, launches of the role and dream runs in the space are held, the space cannot be relocated, and central refuses
  memory under the old name, and under the new one until the node has moved (`role renamed`), without creating a collection for it.
  The node agent speaks minor 14: it checkpoints the role's instance checkouts, sets a folder already under the new name
  aside under `.retained/`, renames the role folder with its checkouts and preserved conflict folders, moves the sync
  state, and reports `ok` (or `deferred` while a run of either name or a dream run is still active on it, moving when it
  ends; or `error`, tried again at its next start) until central acknowledges it; then it syncs the role under the new
  name, from where the old name stopped. Node journal migration `0010_role_renames` (applied on start, forward-only).

- **Control watchdog** (ADR-0099, SM-CTRL-008). A live, idle control that leaves work waiting (unacknowledged
  messages, reviews whose worker finished, ended runs, free worker slots while tasks are ready) for longer than
  `control_watchdog_minutes` gets a one-line reminder typed into its terminal, naming only counts. Reminders back off
  (5, 10, 20, 40 minutes, up to an hour). After `control_watchdog_nudges` unanswered reminders the space's people get a
  human action, and with `control_watchdog_restart` control is also restarted, resuming its session. A working,
  blocked, taken-over or usage-limited control, and a paused space, are left alone. Read-only state:
  `GET /spaces/{space_id}/watchdog` (`get-space-watchdog`), `spaceman space watchdog <space>` and a badge on the
  control row of the space overview.
- **Runs an agent restart stopped are resumed** (ADR-0098). A worker or diagnostic run that holds a task and ends because the node agent
  restarted (`stopped_by_restart`) is resumed by the limit-wait machinery: a new run of the same instance, the same
  worktree and account, `--resume` of its session, after 15, 30 and 60 seconds. At most 3 automatic resumes per chain
  (a resumed run that lived 30 minutes starts the count again); then the task goes back to `ready` and control is told.
  A task already in review, a run somebody asked to stop and control runs are not resumed. Limit waits carry a
  read-only `cause` (`agent_restart`) in the API.
- **A space role has its own description, and control can list roles** (ADR-0095). `spaceman role set --description`,
  `description` on the role API and a field on the web role form; empty falls back to the template's description. The
  role preview shows the effective description with its origin. Control runs get a read-only `spaceman_roles_list`
  tool (SM-CTRL-007): the space's roles with kind, description, template, harness and model, and no prompt text,
  settings, skills or accounts. The description is routing guidance only: it is not in the run's prompt or manifest.
- **Edit a prompt's or skill's name and description in place** (ADR-0095). `PATCH /prompts/{prompt_id}`
  (`update-prompt`) and `PATCH /skills/{skill_id}` (`update-skill`), `spaceman prompt update` and
  `spaceman skill update`, and an **Edit details** dialog on the prompt page. `If-Match` is required. No new prompt or
  skill version is made, so runs see no change in their instructions; the slug and kind stay fixed.
- **Retry or close a stuck role rename** (ADR-0093 addendum). `spaceman role retry-rename` and
  `spaceman role close-rename` (administrators; `--forget-node` gives up delivering a move to a node that is away),
  and `POST /spaces/{space_id}/role-renames/{rename_id}/retry` and `/close`. A closed rename is `abandoned` and leaves
  the node's old folder as an orphan; it is never deleted. A move the node cannot do ends `failed` with its reason.
- **Idle memory checkouts follow the role's shared memory** (ADR-0094). The node fast-forwards the checkout of a live
  run that is between turns, and refreshes a checkout at run start under the same checks, so an instance that waited
  between turns no longer writes on an old base and produces needless `conflicts/` folders. Only an idle, clean
  checkout whose history is an ancestor of the shared head is touched, never with a reset or a merge commit.
  `memory.fast_forward: false` turns it off.

### Changed

- `spaceman role set --skill` accepts a slug, `slug@number` or an ID. `role set` still replaces the role's whole
  configuration; its help now says only the space role's own skills are cleared, not the template's.

### Fixed

_The first entry below was added after the v0.1.13 release._

- **A role rename that races a memory sync no longer leaves a second memory collection under the old name** (#274).
  Central re-checks the rename inside the transaction that creates the collection, so a sync that passed the check
  before the rename committed is answered `role renamed` instead of stranding a new collection under the old name.

- **A run stopped before its launch reached the node no longer stays `pending` for ever** (#270). The node reports it ended
  ("stopped before start") so central closes it through the normal end path, and a launch that arrives after the stop
  does not start it. Stopping a run that has already exited or failed is now a no-op.

- **A recording is acknowledged only once central has stored it** (#242, ADR-0010 addendum). Central acknowledged the highest
  sequence it had accepted, so a failed write could let the node delete the only copy of records central never stored.
  Central now acknowledges what it synced to disk, closes and reopens a failed writer so the node ships the gap again,
  backs off 5 seconds before reopening a recording that failed, cuts a failed segment back to its last sync (removing
  it if nothing was synced) and never ages out a resync mark on a live connection. Reclaiming old recording directories
  that have no journal row remains a design (ADR-0096, not implemented): those directories are still kept.
- **Live terminals stayed empty after an agent restart, and a full disk froze them** (ADR-0097). A restarted agent
  re-shipped the whole recording of every journaled run, ended runs included, one at a time in run-ID order, and held
  back the live output of the runs it had not reached; and the recording writer latched its first write error, dropping
  all later output also for central's copy. Now live runs are caught up first and a waiting run is served between the
  batches of another, ended runs' history is paced at 8 MiB/s, the node persists what central acknowledged
  (`recording-acked` in the run's folder, so a restart re-ships only the rest (the first restart after upgrading
  re-ships everything once, paced)), learns central's sequence before it numbers an adopted run's new output, and a
  full disk no longer stops recording: output keeps flowing to central and the live view, the local recording resumes
  in a new segment after a 5 second backoff (up to a minute), and there is no `disk` `trunc` record any more. The node
  agent needs the update for the full-disk and catch-up behaviour, and central for acknowledging only stored records,
  which the node's saved acknowledgement relies on: upgrade central first, as above.
- **Memory git guard** (security-relevant, ADR-0025 and ADR-0094). The node refuses a memory checkout whose git dir is
  redirected (a symlink, a `gitdir` file, `commondir`, `core.worktree`, a bare checkout), that holds config-based hooks,
  links inside `.git`, gitlinks, a nested `.git` or a FIFO in its work tree, or that lacks its own
  `.git` directory. The checks run first and with timeouts, and cancellation sends SIGTERM first. `memory preserve`
  refuses a symlinked conflicts path.
- **Harness configuration never travels through role memory** (security-relevant, ADR-0025 addendum). Because Claude
  Code loads `.claude/skills` and `.claude/commands` from a directory passed with `--add-dir`, a skill one instance
  wrote into shared memory became active in every instance of the role. The node now excludes and sparse-checks-out
  `.claude/`, `.codex/`, `.agents/` and a top-level `.mcp.json`, also from history an older agent pushed, and hardens
  this against symlinks and hard links. Central keeps history it was already given.
- **A resumed run is told where its memory is after a role rename**, restating the memory folder (also for Codex) with
  a rename line, only for sessions older than the rename.
- **Role rename follow-ups:** a closed rename keeps its move command queued and orders later renames of either name
  behind it; launches wait for a pending rename; a rename onto a name clears the orphan marker; a relocation with an
  unavailable source closes the space's open renames, and a node that prepares a relocated space drops its stale role
  renames (the rename delete is taken under the rename lock, closed rows included).
- **Roles with no profile or a broken profile** are listed by `spaceman_roles_list` instead of dropped, and a role's
  description stays out of run manifests.
- **Starter roles are shared** (documentation): editing a starter template changes it for every space; a space role
  overrides per space.

## [0.1.10] - 2026-11-17

Patch release on top of v0.1.9. **Only the node agents need it**; central can stay on v0.1.9. It has no migrations, no
node protocol change (minor 13) and no configuration change. It fixes Claude Code runs stopping at Claude Code 2.1.326's
new "Adding a new directory" trust prompt before their first turn.

### Upgrade notes

- Upgrade the node agents. Runs already waiting at the prompt stay there: answer it, or stop and relaunch them after the
  upgrade.

### Fixed

- **Claude Code runs stopped at "Adding a new directory".** From Claude Code 2.1.326, a directory passed with `--add-dir`
  must be trusted like the working directory, or the session asks "This session hasn't worked here before. Is this a
  directory you created or one you trust?" and an unattended run waits there forever. The run's memory folder is passed
  that way, so every Claude Code run with memory on 2.1.326 or later stopped before its first turn. The node agent now
  trusts the memory folder too, exactly that folder, in the run's own `.claude.json` (ADR-0021). Only the node agent
  needs the update.

## [0.1.9] - 2026-11-16

**v0.1.8 was tagged but never released** (its release build produced no images), so its changes ship in v0.1.9.

Release on top of v0.1.7. **Central and the node agents both need it**, central first. The main changes are account
switching on usage limits (ADR-0090: a run that reaches its limit moves to another account of its pool, off unless
`usage_limit_failover` is on), Telegram chat threads per space with actionable cards (ADR-0089), the backup hang fix
(ADR-0091: a backup or verification that stops making progress now fails instead of holding `busy` until a restart),
the notifier no longer losing notifications around restarts, text replies to cards, and mobile dialogs (pinch-zoom,
wide tables, 16 px inputs). Memory conflicts now close when reconciled (ADR-0092), and queue policy changes reach control. It brings node protocol
minor 13 (v0.1.7 speaks up to minor 12), schema migrations 0054 to 0060, agent migration 0009 and the configuration keys listed below.

### Upgrade notes

- **Schema migrations 0054 to 0060 are forward-only: take a backup first.** `0054_account_pools` to
  `0058_pool_exhausted_notices` (ADR-0090) add account pools, account switches, a run's failure class, the usage ledger
  and the pool-exhausted notices. `0059_telegram_message_thread` adds the nullable `telegram_messages.message_thread_id`,
  the topic a card was sent in; a card row from before it has no topic recorded and is edited in place as before.
  `0060_memory_conflict_folder` adds `memory_conflicts.folder` (ADR-0092), and the node agent's journal gets its own
  migration `0009`. They are applied on start and need no other action.
- **Node protocol minor 13 (v0.1.7 speaks up to minor 12).** Upgrade central first, then the node agents. An older agent
  keeps working, and its runs still switch on a limit it reports, but only an agent of this release reports the status
  line's usage windows, the cost estimate and the `StopFailure` failure class, so the usage windows, the headroom gate's
  figures and a wrap-up limit's missing reset need the agent upgrade.
- **New configuration keys**, with their defaults (the usage-limit keys are also space limits, set with
  `spaceman limits set --space <space> --set <key>=<value>`, without the `limits.` prefix):
  - `backups.stall_timeout`: `10m`. A backup or verification with no progress for this long fails (1 m to 24 h, ADR-0091).
  - `backups.max_duration`: `1h`. A backup or verification running longer than this in total fails (10 m to 72 h, ADR-0091).
  - `limits.usage_limit_wait_if_reset_within`: `15` (minutes). A run whose account resets this soon waits for the reset
    instead of switching (0 to 1440, ADR-0090).
  - `limits.usage_limit_switch_max_per_day`: `6`. How many times one instance may switch account in 24 hours before it
    waits as ADR-0079 does; `0` never switches (0 to 50).
  - `limits.usage_limit_min_headroom_percent`: `0` (off). A new run does not start on an account whose known window usage
    leaves less than this percentage free (0 to 100). Running agents are never moved because of it.
  - `notification_defaults.threads` (Telegram administrator setting, not the config file): `auto`. See **Chat layout**.
- **Live account switching stays off unless `usage_limit_failover` is on.** That is the existing switch (default off).
  An installation that leaves it off keeps ADR-0079's wait, whatever the keys above say.
- **Chat layout.** Links start on `notification_defaults.threads = auto`, so a bot with topics enabled in BotFather moves each
  chat to a topic per space at its next space message, with one announcement in the chat without a topic. To keep one chat,
  set `threads` to `off`, per link or in the administrator defaults. **Topics the bot creates stay in users' chats**
  whatever you set later: Telegram deletes a topic only together with its messages.

### Added

- **Memory conflicts close when reconciled** (ADR-0092). Control and the dreamer get `spaceman_memory_conflict_list` and
  `spaceman_memory_conflict_resolve` (control: any role of its space; a dreamer: the roles of its dream), audited as the
  run. A conflict also resolves by itself, as the system actor `memory-reconcile`, when an accepted checkpoint removes
  its `conflicts/<name>/` folder; the preserved ref stays. Schema migration `0060_memory_conflict_folder` (applied on
  start, forward-only; the node journal gets `0009`); the node reports each preserved conflict's folder, so the tie is exact; a
  git read error resolves nothing; conflicts recorded before it close through the tool, the CLI or the web UI.
- **Queue policy changes reach control.** When a queue policy write changes `max_active` or an incident setting, control
  gets one system message with each changed field's old and new value (`max_active 2 → 4`) and the audit event; a write
  that changes nothing sends none.
- **Account switching** (ADR-0090, security-relevant). With `usage_limit_failover` on, a Claude Code run that reaches
  its usage limit moves to another account of its pool: at its next idle point it is parked and resumed at once with its
  session (`--resume`) under the next eligible account. Nothing is sent to the running process and no credential is
  written to the instance folder. Control gets one message per switch. A switch's resume re-checks the target and takes
  the pool's next account, or falls back to ADR-0079's wait, when it is no longer usable.
- **Pools:** a space's accounts for a harness form its pool, ordered by priority; an optional role pool replaces it for
  one role. A launch takes the space's default account first when it is eligible; otherwise, and for a role pool,
  accounts are taken by priority, then the fewest live runs, then binding order. A switch ignores the default and takes
  the pool order. Account usage does not rank the pool.
- **Wait if the reset is near:** a run whose account resets within `usage_limit_wait_if_reset_within` waits for it
  instead of switching; beyond `usage_limit_switch_max_per_day` switches an instance waits too.
- **Launch headroom gate:** with `usage_limit_min_headroom_percent` above 0, a launch or resume does not start on an
  account whose known window usage leaves less than that percentage free. Running agents are never moved because of it.
- **Pool exhausted:** when every account of a pool is at its limit, the parked run resumes at the pool's earliest reset,
  and the people get one informational human action per space, harness, role and window (and so a Telegram card). The
  notice is stored with the park, so a crash or a space at its open-action cap delays it but does not lose it.
- **Usage ledger:** tokens and the list-price cost estimate per space, account, role, harness and model, hourly (90 days)
  and daily (730 days), and each account's latest 5-hour and weekly windows. API: `GET /spaces/{id}/usage/series`,
  `GET /accounts/{id}/usage`, `GET /accounts/{id}/windows`, `GET /spaces/{id}/account-windows`, the account pools
  (`GET /spaces/{id}/account-pools`, `PUT /spaces/{id}/account-pools/{role}`) and `GET /spaces/{id}/account-switches`.
  CLI: `spaceman usage space|account`, `spaceman account windows|space-windows|pool`, `spaceman limits switches`.
- **Web:** per-space **Accounts** (pool order, role pools, failover and switch settings) and **Usage** (tokens first, the
  cost only as an estimate, a chart with a table of the same numbers) pages; a run links to the switch away from it and
  the one into it.
- **Evidence:** account switching is tested with simulated harnesses only. It has not yet been verified with two real
  claude.ai accounts (open item K-17).
- Telegram cards are actionable and edited in place (ADR-0089): a question's options, Approve and Deny, Mark completed,
  Acknowledge and Dismiss sit on the card, **More…** turns it into the full form and **Back** returns, and a resolved card
  becomes one line with no buttons. Reminders, escalations and assignments are replies to the card. A card of a space you can
  no longer see is edited to "No longer available here". A card outside its space's topic names the space.
- Telegram home: `/needs` lists every readable space with its total, and a **Needs you** summary is pinned in the chat without a
  topic and edited in place (at most once every 30 seconds). It counts only the spaces and action types the link may view.
  **Known limit:** a `/needs` row opens a form; it doesn't link to the item's card yet.
- A relink of the same chat by the same user reuses the topics of the revoked link, and a space's rename renames its topic.
- The backup status shows where a running backup or verification is (`current`: stage, stage start, files done, last
  progress; `stage` on the running item), and central logs every stage's start and end at Info.

### Changed

- Telegram guide: the section "Topics mode" is now "Chat layout" (the old anchor `#topics-mode` is gone). It no longer
  says a bound group gets notifier cards (it gets none: its replies and taps only), describes `/needs` as cross-space,
  and the setup page has the BotFather steps for topics.
- Telegram: the chat layout is chosen per link (ADR-0089). The new `notification_defaults.threads` setting (`auto`, `on`, `off`; default `auto`, which is on when the bot has topics enabled in BotFather) and a link's own override put every message about a space, needs-you cards included, in that space's topic; the digest and the "many changes" notice stay in the chat without a topic. The shared "Needs you" topic is no longer used: the bot posts a note in it once. The per-space `topics` flag and `topics_mode_default` are deprecated and have no effect. **Topics the bot creates stay in users' chats** when the layout is turned off again: Telegram can only delete a topic together with its messages.

### Fixed

- **The race gate running its runner out of memory.** CI's and the release workflow's `go race tests` died about 15
  minutes in with exit 143 (on v0.1.8 too): `internal/app`'s race test binary peaked at 13-15 GB on a 16 GB runner. The
  `testing` package keeps a finished parallel test, and the cleanup closures it already ran, reachable until every
  parallel test is done, so each test's whole central stayed in memory. The test harnesses now release what their cleanups
  hold (`cleanupReleased`): the binary peaks under 2 GB (live heap about 200 MB instead of 5.3 GB at the end).
  `scripts/race.sh` prints internal/app's peak RSS in its summary, and when it is interrupted it prints the last memory
  sample and the end of internal/app's output into the log, since a runner that goes down skips the artifact upload.
- **Run reports around a node agent's reconnect.** A run report issued between the agent's hello and its welcome went out
  before the protocol minor was negotiated, so a usage limit it carried was stripped; a run report issued between the hello
  snapshot and the connection being set went nowhere, and a run end that had only gone out live was never re-sent. Such
  reports are now held and re-sent after the welcome, and a limit change scanned in that window is held too. A hello that
  fails clears the hold.
- Input the node refuses (a busy or blocked program, a takeover, a run that ended) is no longer lost silently: the run
  page shows a "Message not delivered" toast and the audit log records the reason. Operator input containing control
  characters (C0, DEL and C1, other than tab and newline: escape sequences, carriage returns) is refused with a 400; `\r\n` is read as
  `\n`. A restored space starts control as soon as its lease is granted instead of at the next sweep.
- **Agent journal pruning.** A pruned run's recording directory is now removed with it, once its recording is fully
  acknowledged, and the journal is pruned every hour instead of only after a reconnect, so a long-lived connection no
  longer keeps ended runs and their recordings forever. The same pass removes recording directories that earlier
  releases left behind, but only those with no segments, a well-formed run ID, no journal row and nothing modified for
  24 hours. A recording that has segments and no journal row is kept (a lost journal makes every run look orphaned);
  reclaiming those needs central's confirmation and is not done.
- A Telegram text reply to a notifier card is now about that card. Replying to a question's card answers it (an option's
  letter, number or label, or free text; the same API call, version check and authorisation as its buttons, and a
  closed action says so); an approval is never decided from text; a reply that is not an answer goes to the card's own
  space instead of the chat's current one. A lone letter or number is matched against both an option's position and the
  start of every label ("C" with `Continue`, `Deploy`; "1" with `30 minutes`, `1 hour`): when they name different
  options the bot asks "Which option?" and answers nothing. Any reply to a free-text question is taken as its answer.
- Telegram notifications are no longer lost for changes made while central starts or while it is restarting: the
  notifier reads its start position before the listener starts, and resumes from a saved position (up to 5 minutes
  old) after a restart (ADR-0089 follow-up).
- **Web, phones: pinch-zoom while a dialog is open, wide editor tables, input zoom.** Mantine's scroll lock cancelled
  every two-finger touch while a modal or drawer was open, so the page could not be zoomed (or zoomed back out); pinches
  now pass (`allowPinchZoom`) and one-finger scrolling stays locked. A table in the editor that was wider than its dialog
  was cut off by the dialog's `overflow-x: hidden`; it now scrolls in its own box. Inputs are 16 px on touch screens, so
  iOS no longer zooms the page in when one takes focus. `ui-overflow.spec.ts` opens every dialog in phone emulation.
- The Telegram notifier has a `Ready` channel that closes once it is subscribed and has its start position. The e2e chat
  harness waits for it, because a change made before that is history the notifier doesn't replay: on a loaded runner a
  late-starting goroutine lost the first card and `TestTelegramTopicsModeRoutesCardsAndReplies` timed out.
- A backup or verification that stops making progress no longer hangs with `busy` set until central restarts. A watchdog
  fails it after 10 minutes without progress or 1 hour in total (new keys `backups.stall_timeout` and
  `backups.max_duration`), records it as failed with an error naming the stage,
  and the next backup can start. HTTP/2 connections to object storage now send health pings, so a silently dead
  connection no longer blocks a transfer forever.
- The CI and release race jobs no longer run until the hosted runner is lost. Every `go test -race` invocation has a
  timeout (25 minutes for `internal/app`, 15 for the other packages; CI uses 35 minutes for `internal/app`) and
  `GOTRACEBACK=all`, so a hung test ends as a panic with a dump of every goroutine; the job has a time limit, and its
  test output and a memory log are uploaded as the `race-output` artifact (CI) or `race-output-release` (release) when
  it fails. `internal/app`'s race cap in CI is now 2 (was 4), because its test binary reached 10-12 GB resident and
  exhausted 16 GB runners; the job takes about 14 minutes at the lower cap, measured on a hosted runner.
- Test flakes under heavy node load (test-only; no behavior change). The 100 ms write-lock guard (HP-DB-004) now stretches
  by `1 + load/CPUs` (at most 8x) when more threads are runnable than CPUs, for the default and for
  `SM_TEST_TX_MAX`; an idle machine and `SM_TEST_TX_STRICT=1` are unchanged, and every CI and release job that runs the guard sets `SM_TEST_TX_STRICT=1` (ADR-0062, update 2026-11-16). The S3 trickle test
  uses a 600 ms stall window. The large-recording catch-up test waits
  for progress instead of a fixed 20 s, the StopFailure scan test waits 30 s instead of 5 s for an event-driven report,
  the notifier budget test allows one more timed mapping write per elapsed second, the hello-reapply test applies 120
  journal runs instead of 400, and the fleet e2e cleanup retries emptying the agent directory.
- The node agent's runner reconcile (the monitor's first "snapshot" event, a restarted or missed-events notice) no longer
  marks a run whose launch is still in flight as lost or failed: it leaves those alone and re-checks the runner for the
  others before judging them. Before, a reconcile that landed between the journal's run and the process start ended the
  run, so its bridge calls answered "run is not active".
- The node agent now waits (up to 10 s) for its runner monitor to stop when it shuts down, so the monitor cannot still
  be reading agent state after shutdown returns.

## [0.1.7] - 2026-11-16

Release on top of v0.1.6. It is a central-only release: the node agent is unchanged, so agents can stay on v0.1.6. It has
no migrations, no node protocol change and no configuration change. It fixes the web terminal's reset and "load earlier"
behavior and the Telegram sender and topics, and it polishes the Mini App home and the links on Telegram cards.

### Added

- A Telegram card for a human action whose body is cut at the message cap now has an **Open in app** button, which opens that
  action's page in the Mini App. Cards of other kinds still have no button.

### Changed

- The Mini App home's **Needs you** list is compact: its rows only open, so it shows no selection boxes and no bulk bar.
  The full human-action list keeps its bulk actions.

### Fixed

- After a terminal reset (a seek, or **load earlier**), the web terminal no longer shows the old screen in rows the new output
  has not drawn yet. The new terminal is erased out to the largest size the old one had, so a later resize shows nothing old.
- **Load earlier** puts the viewer's lines back only once the catch-up has reached the record the screen ended on, so they no
  longer end up at the bottom of the scrollback.
- A live terminal whose stream ends or is refused while **load earlier** waits for its records no longer stays "reloading":
  the button comes back.
- Telegram's urgent sends (callback answers) are no longer starved by long cards. A quarter of the sender's worker slots (two
  by default) are kept for Interactive messages, so a multi-message send waiting in the rate limits cannot hold every slot.
- Creating a Telegram topic is now charged to the sender's rate limits like any other Bot API call. The wait for that budget
  no longer holds the link's topics lock, so the link's other topic messages do not queue behind it.
- A human action with a priority the Mini App does not know yet sorts as normal in the **Needs you** list. Before, it had no
  rank in the sort.

## [0.1.6] - 2026-11-15

### Added

- `telegram.webhook_base_url` (environment `SPACEMAN_TELEGRAM_WEBHOOK_BASE_URL`): the public https base URL the Telegram
  webhook is registered under, for a deployment whose `public_url` is not reachable from the internet. Empty keeps the
  webhook under `public_url`. The webhook path and secret-token check are unchanged.
  A path prefix is for the reverse proxy, which must strip it before forwarding (central serves only
  `/telegram/webhook/<id>`). The host must differ from `telegram.miniapp_host`.
- The Telegram Mini App home leads with a cross-space **Needs you** list: the total number of pending human actions, the five most urgent (then oldest) with their space names, and a See all link to the full list. A `startapp=needs` deep link opens it. No new API.

### Changed

- Telegram cards now show a message body as formatting (bold, code, lists, links) instead of raw Markdown, and send the whole body instead of cutting it at 600 characters: what doesn't fit in the card goes in numbered reply messages ("(2/3)"), up to four messages, and the rest is left to the app. The continuation messages are edited, added or deleted when the card changes, each call within the sender's rate limits, and a deleted one is not remembered after a restart. A message card says "Unread" or "Read" instead of the internal state "stored". At content level `summary` a card still never shows a body.
- Control can close a task that never had a worker run (control did the work itself) as **done** straight from backlog or ready with `spaceman_task_set_status`, a result required; the audit event is marked `direct`. The task must have no unmet dependency and no open `blocks_task` human action. No other transition changes, and operators and workers cannot use it.
- Developer tooling: `make e2e-up`, `make e2e-spec SPEC=… GREP=…` and `make e2e-down` run one e2e spec against a stack that stays up; `make e2e` and `make screenshots` stop their stack on failure and Ctrl-C; the dev stack picks free ports when the defaults are taken and drops `SPACEMAN_*` variables; `ui-overflow.spec.ts` checks its three widths in parallel (12.3 to 5.1 minutes).

### Fixed

- Telegram Mini App writes (POST, PUT, PATCH, DELETE) are no longer refused when `public_url` is plain http or has a port. The one allowed origin is now `https://<miniapp_host>`, the URL the bot's menu button opens, instead of one built from `public_url`'s scheme and port. The check is still exact: one origin, no wildcard. Mini App buttons and download links now use `https://<miniapp_host>` too.
- Attaching a file or image in the Telegram Mini App (new inbox item, human-action responses, messages, tasks) no longer fails with "authentication required". The upload sent a cookie and CSRF token, which the Mini App never has; it now sends the Mini App's `Authorization: Telegram` session like every other call. An inline image is previewed in the composer and stored as an attachment reference.
- A Mini App live stream that is refused with `mfa_required` now shows the finish-MFA message instead of the generic reopen screen.
- The Telegram Mini App settings page no longer says topics mode is "Not available yet". It shows, read-only, which of your spaces use topics and what to do in BotFather if Telegram refused a topic in your chat. `get-my-telegram-link-prefs` reports `topics.spaces` and `topics.refused`, and `topics.available` is true while the bot runs.
- The restore banner now goes away when an administrator dismisses it. Before, the Dismiss button only hid itself and the banner stayed while any space was paused, even on purpose. A new restore shows it again. The `acknowledge-restore` operation is now also an admin MCP tool (a dangerous one, so it needs approval).
- The human-action lists sort before they truncate, so a top-N view (the Mini App home) shows the most urgent pending action even when more than a page is pending; before, a newer urgent item was cut off and the oldest were kept.
- Admin MCP tools whose operation requires `If-Match` (`set_space_state`, `update_space`, and the other versioned writes) list `if_match` as a required argument and say so in the description, so an approved call is not refused with a 428 and approved again. Operations where it is optional (a missing version means create or overwrite, e.g. `move_task`, `set_space_limits`, the draft saves) keep it optional and make no 428 promise.
- `save-harness-profile` and `save-role-template` document `If-Match` as optional: creating one does not read it. Updates still require it (428 without).
- A question request refused with `unknown field "allow_text"` was a request of another type (approval, to-do, message), where `allow_text` is not a field. The refusal now names the type and the fields it takes, and the `spaceman_human_ask` request description says `allow_text` is for questions only.
- After a restore or reconnect, a node agent no longer reports ended runs twice or in a burst: the reconcile's inline report skips runs the connection's paced follow-up has yet to send, and runs it already sends.
- Central's shutdown waits for in-flight run relocation steps, so one can no longer run against a closing store. A relocation kicked after shutdown began resumes from the database at the next start.
- On phones the run message box matches the height of its Send and inbox buttons (16px font, so iOS Safari doesn't zoom on focus), and the run message bar controls line up.

### Security

- The response write bound (`http.write_idle_timeout`) skips only the node WebSocket route. A download or event stream
  that carried WebSocket upgrade headers used to be exempt from it.

## [0.1.5] - 2026-11-15

Release on top of v0.1.4. It adds the Telegram integration (ADR-0083): a bot you chat with, a Mini App that opens inside
Telegram, topics and group mode. All of it is built and tested against a fake Telegram server and none of it is verified
against a real bot yet, so the release makes no production-readiness claim. It also adds usage-limit handling for Claude
Code runs (ADR-0079). **Central and the node agent both need upgrading**, central first: central adds migrations 0050
to 0053, and the node agent adds its journal migration 0008 and node protocol 1.12. Telegram needs one new setting,
`telegram.miniapp_host`.

**Upgrade notes:**
- **Upgrade central first and check that it starts** before any node agent. Central's migrations since v0.1.4 are 0050
  (usage limits), 0051 (every Telegram table; it rebuilds `role_grants` with its rows kept), 0052 (link-code confirmation)
  and 0053 (the leaked-code signal). They are forward-only in production: take a backup first, and to go back restore a
  snapshot taken before them.
- **Upgrade the node agents after central.** The agent's journal gets migration 0008 (a run's reported limit), and the node
  protocol goes from 1.11 to 1.12, whose run reports may carry the usage limit a node detected. A node agent below 1.12
  keeps working as before, without usage-limit detection. The journal now deletes ended runs after `journal_retention`
  (default `720h`).
- **Restarting a node agent ends its runs**, as before: let them finish or pause the node's spaces first.
- **New configuration:** `telegram.miniapp_host` (a lower-case DNS name, the host the Mini App is served on; with no host
  set, the Mini App fails closed). Also new are `telegram.api_base_url`, `http.stream_heartbeat`,
  `usage_limit_max_wait_hours` and `journal_retention`, each with a default.
- **Telegram stays off until it is set up:** an administrator configures the bot, and someone with `space_config:write`
  turns on each space (`enabled`). Nothing changes for a deployment that doesn't do either.

### Added
- Telegram Mini App watch-only terminal (ADR-0083, TG-10-3; the bot isn't usable yet): off by default. A space's admin turns on
  `terminal_watch` in its Telegram settings, and a run page in the Mini App then shows the live terminal of that space's
  runs to links that may watch terminals (operator and up). It is read-only: no keys, paste, size or takeover, and no replay.
  The stream comes straight from central over the Mini App host, never through Telegram; the server refuses it again
  unless the space opted in and the link holds `terminal:watch`, and a stream already open ends (`forbidden`) at the next
  re-check once the setting is turned off. New `get-telegram-space-features` tells the Mini App
  whether to show it.
- Mini App: your Telegram settings page is editable (notification categories, quiet hours, digest, muted spaces and your
  time zone, which is per user; topics are shown read-only). Saves send `If-Match`; a stale save reloads the form with a
  conflict notice. The queue page shows the read-only "Next" card (never `record=true`).
- `move-task`, `ack-message` and `resolve-message` accept an optional `If-Match` with the resource version; a stale version is a
  409 `precondition_failed` and changes nothing, and requests without it behave as before. The Telegram chat sends the card's
  version with them, so a change between its check and the call is a conflict, not a stale action.
- Telegram topics mode (ADR-0083, TG-11; the bot isn't usable yet): a space whose Telegram settings turn `topics` on gets a
  topic of its own in each linked private chat, and its open actions go to a "Needs you" topic. The bot creates a topic on
  first use and remembers its thread per link. Chat replies go to the topic the user wrote in. A topic that was deleted
  or closed is created again once; if that fails, or the chat has topics off for the bot, the message goes to the main
  chat. Topics only choose where a message appears; who may receive it is unchanged.
- Telegram group binding (ADR-0083, TG-13-2): `put-space-telegram` takes `group_chat_id` to bind a forum supergroup to a
  space (`0` unbinds; left out keeps it), for space admins only (`telegram_group:write`). The bot must be a member of a
  supergroup with topics (422), a group serves one space (409), and `spaceman telegram` gets `--group-chat-id`. In a
  group, refusals and answers to button taps are alerts only the tapper sees, a refused command gets one short reply
  that shows nothing, forms link to the Mini App with a `t.me` direct link (Telegram refuses app buttons in groups), and
  posts to a group take the `send_limits.group_per_minute` limit. The bot stays in a forum supergroup until it is bound
  (it still leaves chats that can never be bound) and leaves a group that loses its binding.
- Telegram group mode authorization (ADR-0083, TG-13-1): in a forum supergroup
  bound to a space, every button tap and command acts as the tapping person's own linked account and grants, never the
  binder's or the group's. Members without a link, or whose account can't use the space, are refused; nothing outside the
  bound space is reachable; forms and confirmations belong to whoever opened them; enforced approvals are never decided
  in a group. Only summaries are ever posted to a group, whatever the space's content level, and files never are.
- **Telegram bot and Mini App (ADR-0083), documentation.** A guide (the site's Telegram page) covers linking an
  account, the bot chat (spaces, inbox capture, queue, runs, messages, control), needs-you forms and approvals (including
  the per-space opt-in for enforced approvals), the Mini App, topics mode, group mode, notifications with quiet hours and
  the digest, and the administration settings. An operator page (Telegram setup) covers
  `telegram.miniapp_host` (it must match the bot's host, and it fails closed without it), webhook versus polling, the
  trusted proxy, and that the planned Terraform `spaceman_telegram_bot` needs central's `miniapp_host` set first. LIMITATIONS lists what
  Telegram can't do (web only, no `web_app` buttons in groups, an opt-in watch-only terminal, summary-only groups), and
  SECURITY and the threat model cover the link principal, limited links for pending MFA, single-use bound confirmations,
  opt-in enforced approvals, fail-closed Mini App hosts and the stream heartbeat. **The bot and Mini App are tested against a fake
  Telegram server only; nothing is verified against a real bot yet.** The guide also covers topics mode, group mode
  (binding by an administrator, summary-only, alerts only the tapper sees, silent on foreign commands, when the bot
  leaves), the watch-only terminal, the editable Mini App settings and the command menu (`/needs`, `/help`, `/settings`;
  there is no `/new` or `/digest`).
- Telegram link preferences (ADR-0083 amendment TG-10-4; the Mini App page that edits them comes separately): two Mini
  App operations read and save the notification preferences of your own Telegram link (categories, quiet hours, digest,
  muted spaces) and your time zone. The link is always the session's own; nothing else can be changed there. The Mini App
  may also read what the queue picks next (`queue-next`), without recording the decision.
- Telegram needs-you common actions (ADR-0083, TG-7-3, not usable until the bot ships): every pending card can be
  claimed or released, commented on, answered with a question to the requester (a clarification, which never answers the
  request), and withdrawn after a confirmation tied to that card's version. A photo or file sent as the reply to a comment
  prompt becomes an attachment of the comment, and the card's attachments come back to the chat with forwarding and saving
  off, where the space shows content. A card that went stale gets a conflict and is redrawn, never overwritten.
  Approvals still can't be withdrawn from Telegram.
- `http.stream_heartbeat` (default 15 s, 1 s to 45 s) sets the heartbeat interval of the change feed and the terminal stream, so a
  proxy that drops idle connections (about 100 s) never sees them idle.
- Telegram queue, runs and messages (ADR-0083, TG-8b; the bot isn't usable yet): `/queue` shows what runs, what is ready
  and what waits, with status changes, placement (top, up, down, bottom) and incident decisions after a confirmation;
  `/runs` shows the live runs and stops one after a confirmation (the API requires `run:control`); `/messages` lists what
  agents sent to the operators, to reply to, acknowledge or resolve. `/control` turns plain text into drafts you send to
  the space's control run; its replies come back to the chat. There is no task creation or run launch in chat.
- Telegram needs-you forms (ADR-0083, TG-7-1, not usable until the bot ships): `/needsyou` lists what waits for you; questions
  (options and free text), todos (steps), messages and custom types with a simple response schema (a step-by-step wizard)
  are answered in chat as you, with the same checks as the web. Approvals stay in the app.
- Telegram approvals (ADR-0083, TG-7-2, not usable until the bot ships): an advisory approval is decided in chat with
  Approve → Confirm, and a denial asks for a note. An enforced approval stays in the browser unless its space sets
  `enforced_approvals = confirm`; then it takes a second confirmation in chat, checked by the server. Withdrawing an
  approval from Telegram stays refused.
- Telegram inbox capture (ADR-0083, TG-8a-2; the bot isn't usable yet): a message to the bot becomes an inbox item in
  the chosen space. Text or a caption is the body, photos and files become attachments (up to 20 MB, through the API's
  upload checks), a forward adds a "Forwarded from" line to the body, and an album becomes one item. Every capture is
  answered with an ack. A webhook update is acknowledged before it is handled, so an update in flight when central
  restarts is lost together with its ack; send it again. Reading an item in chat also sends its first attachments,
  with forwarding and saving off.
- Telegram notifications (ADR-0083, TG-6-2, not usable until the bot ships): quiet hours send silently, reminders, escalations
  and assignments to you arrive as their own messages, a daily digest is sent at each user's own local time, and the bot's
  message mappings are pruned after 30 days.
- Configuration: `telegram.api_base_url` (default `https://api.telegram.org`), the Bot API endpoint for the planned
  Telegram bot (ADR-0083). It is settable only in central's configuration, never through the API. Nothing uses it yet.
- Telegram groundwork (ADR-0083, not usable yet): migration 0051 with every Telegram table, and the inbound side of the
  bot: a hardened webhook route `POST /telegram/webhook/<hook id>` outside the API (secret token checked in constant
  time before the body is read, a refusal before the body closes the connection, a 10 s real-clock read deadline,
  256 KiB limit, `update_id` dedupe, a bounded queue that answers 503 when full), a long-polling mode, and a dispatcher that serves private chats only and tells unlinked users how to link, at most
  once an hour. The route answers 404 until bot configuration ships. New metrics `spaceman_telegram_webhook_requests_total`,
  `spaceman_telegram_updates_total`, `spaceman_telegram_updates_queued` and `spaceman_telegram_poll_errors_total`.
  **Migration 0051 must follow 0050** (the usage-limit work): it is released only after it.
- Telegram bot configuration (ADR-0083, not usable yet): `get-telegram-bot`, `put-telegram-bot` and
  `delete-telegram-bot` (`/api/v1/telegram/bot`, CLI `spaceman telegram bot get|set|delete`; system:read to read,
  system:write with a step-up to change). The bot token is write-only: checked with Telegram's getMe, sealed, and never
  returned, logged or audited (reads show `has_token` and `token_version`); the webhook secret is generated and rotates
  with each token. Settings: `enabled`, `mode` (webhook, which needs an https `public_url`, or polling),
  `miniapp_host` (a lower-case DNS name, not an IP address or the public URL's host) and `webhook_ip_allowlist`
  (canonical CIDRs). Central brings the bot up at start and on every change (commands, menu button, then `setWebhook`
  or polling) and retries a failed start. Configuring a different bot revokes every Telegram link. The System page and
  `get-system` show the bot's health from `getWebhookInfo`.
- Telegram settings (ADR-0083, TG-3c; the bot itself is not usable yet): `get/put/delete-telegram-settings` for the link
  policy, link-code and Mini App lifetimes, send and inbound limits, notification defaults and the topics-mode default,
  and `get/put/delete-space-telegram` plus `list-space-telegram` for each space's opt-in, content level, enforced
  approvals, topics and terminal watch. Every write needs `If-Match` and a step-up and is audited; a field left out
  takes its built-in default and only what was set is stored. Changes apply without a restart. Spaces stay invisible to
  Telegram until enabled. Linked users over the inbound action limit get one "slow down" answer (new outcome
  `throttled` on `spaceman_telegram_updates_total`).
- Notification preferences: an optional `timezone` (IANA name) on the Account page. The planned Telegram digest and
  quiet hours use it and stay off until you set one; there is no server-wide fallback.
- Metrics: `spaceman_telegram_*` for the outbound queue of the planned Telegram bot (sends, drops, 429s, overflow
  notices, queue depth and wait; ADR-0083). Nothing sends through it yet, so they stay at 0.
- Telegram security core (ADR-0083, not usable yet): the Telegram principal acts as its link's capped subject
  intersected with the user's current grants, never with step-up, own-account operations or a token. Every API
  operation, raw route and the change feed has a reviewed Telegram class (a new one is unreachable from Telegram until
  classified). The main host answers `Authorization: Telegram …` with 401 before any cookie is read; the Mini App's
  Telegram mode of the API accepts only that scheme, drops cookies and never sets one. Approvals can't be decided
  from Telegram. Migration 0051 also rebuilds
  `role_grants` (rows kept) to allow link grants.
- Telegram linking, protocol core (ADR-0083 decision 2, not usable yet: no chat surfaces): the Account page's
  operations `create-telegram-link-code`, `get-telegram-link-code`, `cancel-telegram-link-code` and
  `confirm-telegram-link` (`/api/v1/me/telegram/link-codes`). A browser session with a recent re-authentication (never
  an API token) creates a single-use code (only its SHA-256 stored, `link_code_ttl_seconds`); `/start <code>` in a
  private chat with the bot binds it to that Telegram account and asks it to confirm, naming the server's host and the
  account's display name and masked e-mail ("This wasn't me" cancels and is audited); only then, and only from the
  session that created the code (a re-authentication carries it, a logout or new sign-in doesn't), does the web
  confirmation make the link. Its grants are the user's grants at that moment capped by the link policy
  (`link_default_max_role` as a ceiling, admin only with `link_allow_admin_cap`, SSO grants only from an SSO session
  with `link_sso_grants`). Migration 0052 adds `tg_confirmed_at` and `created_session_id` to link codes. New outcome
  `link` on `spaceman_telegram_updates_total`.
- Telegram Mini App page and downloads (ADR-0083, not usable yet): the Mini App host serves `tg.html` and the build's
  assets with their own headers (`script-src` pinned to Telegram's loader by its exact URL, `frame-ancestors
  https://web.telegram.org`, no `X-Frame-Options`); the main host never serves `tg.html` and its headers are
  unchanged. `create-telegram-download-link` (`POST /api/v1/telegram/download-links`) mints a link to one attachment for
  the caller's Telegram link, and `GET /tg-dl/<id>` on the Mini App host serves it: random ID kept by hash, valid 60
  seconds and 3 requests, the link's permissions checked on every use, `Cache-Control: no-store`, `Referrer-Policy:
  no-referrer`, the one CORS header (`https://web.telegram.org`) of the host, and `/tg-dl/<redacted>` in logs.
- **Runs survive Claude usage limits** (ADR-0079, Accepted; SM-RUN-004..006). When a Claude Code run reaches its
  subscription's 5-hour or weekly limit, or a Harbor Pilot gateway runs out of accounts or budget, SpaceMan holds the
  account until the reset, keeps a short wait alive, parks a long one with its session and task, resumes it at the
  reset with `--resume`, and tells control and (for waits over 12 hours) the space's people once. The entries below
  give the details. New guide page "When an account runs out", and a "Usage limits" section in Known limitations.
  Read it before you rely on this: no live subscription limit has been observed on a SpaceMan run, so the tests are
  simulated (scripted node, fake transcript records, fake clock, fake Harbor Pilot; the gateway side also ran against a
  real Harbor Pilot demo, still with a scripted node); a gateway reset is an estimate from Harbor Pilot's quota board;
  and Codex runs are not covered.
- Usage limits, groundwork (ADR-0079): central migration 0050 (a run's reported limit, the `usage_limits` and
  `limit_waits` tables), the agent's journal migration 0008, and node protocol 1.12, whose run reports may carry the
  limit a node detected. Both migrations are forward-only in production: take a backup first. Upgrade central first,
  then the node agents; a node agent below 1.12 keeps working as before.
- Usage-limit detection on the node (ADR-0079): when a Claude Code run's turn ends, and when it ends, the
  node agent reads the typed usage-limit fields of the run's transcript (from where it stopped last time) and reports
  the limit (kind, reset time, since) or its clear with the run, once per change. No transcript text is logged or
  sent.
- Usage limits are visible (ADR-0079): central stores the limit a node reports for a run (`limit` on runs, and in
  control's `spaceman_runs_list`). It opens a hold on the run's account, or on the Harbor Pilot connection or project,
  and clears the hold when a run on that account reports the limit over. A run that ends at a limit ends with
  `usage_limit`. New reads: `GET /spaces/{id}/usage-limits` (including other spaces' holds on the same accounts),
  `GET /spaces/{id}/limit-waits` and `GET /accounts/{id}/usage-limits`, also available as `spaceman limits holds`,
  `spaceman limits waits` and `spaceman account holds`. New events: `fleet.usage_limit.reached` and
  `fleet.usage_limit.cleared`.
- Runs waiting for a usage limit to reset are kept alive and protected (ADR-0079 decision 3): a worker or diagnostic run
  at its limit no longer counts against the space's concurrency limit (it still counts against the node's capacity),
  gets no terminal notices until the limit clears (its messages wait, then are noticed), and control's idle stop is not
  armed while control itself is at its limit. Control gets exactly one message when a run reaches its limit and one when
  the limit clears, written with the change that causes it, never once per report.
- Usage-limit holds block launches (ADR-0079): a launch or resume on an account (or Harbor Pilot connection or project)
  that any space holds at a usage limit is refused (429) with the reset time in UTC, or with the next re-check time when
  the reset is unknown; a refusal writes nothing. When a hold with an unknown reset is due for its re-check, exactly one
  launch goes through as the probe; the next re-check backs off from 1 h, doubling, up to the keep-alive maximum. A
  control run that ends at a usage limit is no longer restarted, counted against the restart budget or reported to
  people, and control auto-start waits for the reset. Two new space limits: `usage_limit_failover` (off by default)
  lets a launch use another bound account of the same harness without a hold (never a direct account for a space routed
  through Harbor Pilot), and `usage_limit_keep_alive_max_minutes` (default 360). "Clear now":
  `POST /spaces/{id}/usage-limits/{limit_id}/clear` (`spaceman limits clear`, run:control) clears only that space's
  hold and says when other spaces still hold the account; `POST /accounts/{id}/usage-limits/clear`
  (`spaceman account clear-holds`, account:write) clears every space's holds on the account. Both are audited, and
  each cleared space's control is told once.
- Runs at a usage limit are parked and resumed at the reset (ADR-0079 decisions 4–6). A run whose reset is further away
  than the keep-alive is stopped at once, one still limited after the keep-alive is stopped then, and one that ends, is
  lost or is swept while at its limit is parked too (control, worker and diagnostic runs). At the reset, SpaceMan
  resumes it as a new run of the same instance with `--resume` of its session (a fresh session with its original
  prompt when the space moved node), and its task points at the new run. Control resumes first, then the oldest, at
  most five every 10 seconds. A wait that can't resume yet (node away, space paused or full, account still held) keeps
  waiting, and the node's hello or the space's resume brings it forward. After `usage_limit_max_wait_hours` (new
  space limit, default 168) a worker's task goes back to the queue, ready and unassigned; control is never given up
  on. Control gets one message per park, resume and release, in place of the run-ended notice, and a parked control
  isn't auto-started. A task that is finished, cancelled or reassigned ends its run's wait. New endpoints
  `POST /spaces/{id}/limit-waits/{wait_id}/resume` and `.../release` (`spaceman limits resume|release`, run:control),
  audited. A hold that clears lets the runs parked under it resume on the next round. A resume whose `--resume` fails
  before the first turn starts once more in a fresh session. A released task in review stays in review, with only its
  assignee cleared.
- Harbor Pilot runs waiting on an exhausted gateway now know roughly when to come back (ADR-0079 decision 9). Central
  estimates a gateway hold's reset from Harbor Pilot. For a quota exhaustion it takes the earliest reset among the
  quota board's exhausted accounts (`GET /api/v1/quota/board`, which the service-principal token's `usage.read`
  already allows). For a blocking budget it takes the budget's period end. It reads Harbor Pilot again every 15
  minutes, or at once when a subscription on the Harbor Pilot quota template receives `quota.window.recovered`. When
  the accounts recover or the budget no longer blocks, the hold clears and the parked runs resume. Launches refused
  meanwhile name the estimate. The board covers every account of the Harbor Pilot installation, not only the ones the
  space's plan uses, so the time is an estimate: a run may resume early, hit the limit again and be parked again.
- People are told about long usage-limit waits (ADR-0079 Q5): when an account (or the Harbor Pilot gateway) is held
  until a reset more than 12 hours away, the space gets one informational human action naming the account, the reset
  and what waits on it, once per space and limit window. New space limit `usage_limit_notify_after_hours` (0–720,
  default 12; 0 turns it off). The `usage_limit_*` limits are validated like every limit, and the limits API now lists
  `hours` among the units.
- Usage limits in the web UI (ADR-0079): a run at its usage limit shows the limit and its reset time (and "at usage
  limit" as its state), and a run parked at one says so. The Limits page lists the accounts and gateway references held
  at a usage limit (other spaces' holds on the same account as "shared", without their details) and the runs waiting
  for a reset, each with until when, plus "Resume now", "Release" and "Clear" for operators and "Clear in every space"
  for account admins. Times are shown in the viewer's time zone, with UTC in the tooltip. The usage-limit maximum wait
  now reads in days and hours on the Limits page.
- Rich Markdown in every rendered body (inbox items, tasks, human actions, messages and the rest): footnotes, heading
  anchors, highlighted code blocks with a copy button, and Mermaid diagrams in the app's light and dark theme. The
  highlighter and mermaid load only when a body needs them (ADR-0082).
- Web: a space's overview has a **New inbox item** action next to **Launch run**, for users who may add inbox items
  (operators and admins of the space). It opens the Inbox page's dialog, and a toast confirms the new item with a link
  to it. On a phone the action is an icon. An unsent draft now survives closing and reopening the dialog.
- Web: a run's terminal view (the run page and the Terminals views) has **New inbox item** next to **Take over**, and
  **Send as inbox item** next to the message box's **Send**. The latter opens the same dialog prefilled with the text
  and a link to the run, sends nothing to the terminal and clears the message box once the item is added. Both use the
  same permission as the Inbox page and need no takeover; on a phone they are icons.

### Fixed
- The Telegram Mini App host fails closed (ADR-0083): new configuration `telegram.miniapp_host`
  (`SPACEMAN_TELEGRAM_MINIAPP_HOST`). A request for that host, or for the bot's stored `miniapp_host`, is served only
  by the Mini App and answers 404 for everything while no bot runs or it is disabled; it never reaches the main API,
  sign-in or web UI. When the configuration and the bot's `miniapp_host` differ, the configuration wins and the System
  page shows the difference.
- The client address behind a trusted proxy (`http.trusted_proxies`) now reads every `X-Forwarded-For` header
  line, not only the first, so a proxy that appends its own line can't be skipped over (audit source addresses and the
  sign-in rate limit).
- **Flaky write-lock guard under `go test -race`.** `TestGatewayRevoked*` (and others) failed with write locks held
  100-300 ms against the 100 ms limit when run directly under the race detector on a busy machine. Not a long
  transaction: purging a space, for one, spends about 60 ms in the cascade over its ~40 child tables with the detector
  on, and tens of ms without it. A store opened by `storetest.Open` in a race-built binary now defaults to the 500 ms
  that `make race` already used; `SM_TEST_TX_MAX` still overrides it, and runs without the race detector keep 100 ms.
- A run could show `starting` with no terminal while its program was running. A repeated launch made the node report an
  older copy of the run, which could reach central after the `running` report and replace it. Central now ignores a run
  report that is behind the state it already has (ADR-0086).
- A terminal you are only watching (live, replay or someone else's takeover) no longer focuses its input: touching it on
  a phone does not open the on-screen keyboard, and key presses are not captured or swallowed. Scrolling, selecting
  and copying still work. Taking a terminal over puts it in input mode, and ending the takeover takes it out again.
- Stopping the node agent now waits for the run launches and stops it already accepted. Before, a launch could still
  be writing the run's files and its journal after the agent had stopped, while the next agent process started.
- Central's shutdown now waits for node connections and the work they started (memory syncs into the role-memory
  repositories, usage, transcripts, tool calls, relocation transfers) before it closes its database. Before, that work
  could still write after the database was closed. The wait shares the 5 s gateway mints already had, so a stop still
  takes at most 43 s (30 s `shutdown.timeout`, 5 s, 8 s for the database), inside the units' `TimeoutStopSec=45s`.
  Central also refuses new node connections once shutdown has begun; a memory sync still queued when its connection
  ends is dropped (the node sends it again) and a tool call ends with its connection.
- **A message typed into "Message the agent" (for example to control) while the agent was busy was lost.** The box sent
  safe, idle-gated input; the node refused it while the program worked, and nothing tried again. The text is now typed
  into the running session at once (the fake runner queues typed input; real Claude Code and Codex are not yet verified to). The placeholder
  and the guides say so (ADR-0084). Messages from the Messages page and agents are unchanged: stored, then announced
  by the terminal notice when the program is idle.
- Resuming a paused space now starts control when work was waiting. The resume did not wake the control notifier, so
  work that arrived while the space was paused started control only at the next slow sweep. Resuming kicks the
  notifier; auto-start settings, limits and dedupe apply as before.
- The "Restored from a backup" banner no longer comes back when an operator pauses a space later: only spaces the
  restore paused and that have not been resumed since count as still paused (the notice now records `resumed_spaces`).

### Changed

- The node agent's hello carries only live and adoptable runs and the unacknowledged ended runs that are under 24 hours
  old, instead of every run the node ever had (a node with 2500 ended runs sent a 1.1 MB hello on every connect). Older
  unacknowledged ended runs follow the welcome in slices. An ended run that central's reconcile still lists as active
  (a restored backup, a report lost with the connection) is reported again at once. No protocol change.
- The node journal deletes ended runs after `journal_retention` (default `720h`, 30 days to 1 year), once central
  acknowledged their final state, their recording is fully shipped and their transcript upload is finished. Live and
  adoptable runs, and the runs that Claude Code cache GC's pins come from, are kept. See ADR-0078.
- A run's terminal opens on its newest output. The live view and replay load only the last 1 MiB of a run's recording
  first, with the terminal size and modes in effect there, instead of the whole recording (tens of MB for a long
  session). **Load earlier output** at the top of the scrollback fetches more on demand. Replay seeks draw at most 1 MiB
  before their position. Central streams catch-ups and recording pages instead of reading whole recordings into memory
  for each viewer. The API adds optional `tail` parameters to the terminal stream and the recording endpoint; older
  clients and servers keep working (ADR-0081).
- Central reads at most `http.catch_up_concurrency` stored recordings at once (new setting, default 8). A terminal
  catch-up or recording page waits up to `http.catch_up_wait` (default 5 s) for a slot, then gets 503 with
  `Retry-After`; the web UI retries. A recording page's `gaps` now cover the records up to the end of the page.

### Security

- **Reserved dedup namespaces** (ADR-0087). The dedup keys SpaceMan's own services use for human actions
  (`approval:`, `usage-limit-human:`, `control-failing:`, `control-autostart-*:`) and tasks (`xspace:`, `schedule:`)
  are refused from the API and agent tools with `validation_failed`. Before, a caller could create a row under such a
  key first and the service would reuse it: an enforced approval's action could show text the approvals service never
  wrote. An enforced approval also refuses an existing action under its key that it didn't write, revokes itself, and
  is asked again under a new ID.
- Central's public listener no longer lets a client hold a connection by declaring a request body and not sending it.
  A handler that refused such a request without reading the body (a 401, a POST to a UI path) made the server wait,
  with no deadline, for up to 256 KiB that never came, and a handler reading a body the client stopped sending waited
  forever. Every read of a request body must now bring data within **`http.read_idle_timeout`** (new, default `30s`,
  1s to 10m): a large upload that keeps moving is never cut off. A response sent before the body was read to its end
  closes the connection instead of draining the body. A body must also arrive within its operation's own bound
  (5 s for JSON operations, 10 minutes for attachment uploads). Requests without a body (event streams) and real
  WebSocket handshakes are unaffected; `Upgrade` headers on a request with a body exempt nothing (ADR-0085).
- A kept-alive connection waiting for its next request is closed after **`http.idle_timeout`** (new, default `2m`, 1s
  to 1h); before, it could wait forever.
- The Markdown sanitizer is stricter: images render only from attachments, and buttons, form fields other than task
  checkboxes, `<link>`, `<meta>`, `<base>`, frames and `srcset` are removed. Rendering a deeply nested or
  delimiter-heavy body is bounded to a few seconds. Mermaid renders in strict mode, its SVG is sanitized again (images
  in labels removed) and a diagram can't change its theme. Diagrams are bounded too: a large one renders only after a
  "Render diagram" click, and one over 500 statements, or past 25,000 characters of diagrams in one body, shows its
  source (ADR-0082).

## [0.1.4] - 2026-11-14

Patch release on top of v0.1.3. It makes no production-readiness claim (docs/RELEASE_READINESS.md §18). It contains:
- recordings: a burst of terminal output over the protocol message limit, or output sent just before a node lost its
  connection, no longer goes missing from central's copy of a run's recording (#56);
- the web terminal keeps at least 10,000 rows of scrollback, instead of about 300 at 160 columns (ADR-0080, #53);
- central and the node agent refuse to start on a database that is missing a lower-numbered migration, instead of
  skipping it silently and failing the next upgrade (#51).

**Upgrade notes:**
- **From v0.1.3:** no new migration, no protocol change (node protocol stays 1.11) and no new setting.
- **Upgrade central first and check that it starts** before upgrading any node. Central and every node agent now check
  their database at startup: one that already ran a higher migration than a lower one in the binary refuses to start
  with `database is missing migrations lower than its schema version` and names each missing migration. A database
  that has only ever run released versions has no such gap; one that ran a development build might. The fix is to
  restore a backup or pre-migration snapshot taken below the lowest missing version (Troubleshooting in the operator
  docs); never insert the version row by hand. A node's journal gets the same check when its agent restarts.
- **Restarting a node agent ends its runs**, as before: let them finish or pause the node's spaces first.
- **From v0.1.1 or v0.1.2:** the v0.1.3 upgrade notes below apply as well.

### Fixed

- Central and the node agent's journal now refuse to open a database that is missing a migration lower than its
  schema version, with `database is missing migrations lower than its schema version` naming each one. Before, a
  migration that arrived after a higher one had already run (branches merged out of number order) was skipped
  silently and the next upgrade failed with goose's "missing (out-of-order) migration". Migrations still run in
  number order only. A database restored from an older backup has no such gap and migrates forward as before.
- The web terminal keeps at least 10,000 rows of scrollback at 160 columns (about 11,000-12,000). ghostty-web's
  `scrollback` option is a byte limit, not a line count, so the old setting of 10,000 kept only about 300 rows at 160
  columns. It is now 24 MiB: a terminal with a full scrollback of plain text holds about 30 MiB of WebAssembly memory
  (ADR-0080).
- A burst of terminal output larger than the protocol message limit (4 MiB, e.g. megabytes printed within a second)
  no longer goes missing from central's copy of a recording. The node sent it as one message, central closed the
  connection, and the records in it were never sent again; the reconnect catch-up's batches could do the same. The node
  now splits recording shipments into messages of at most 1 MiB.
- Records sent just before a node lost its connection could still go missing from central's copy, because central
  keeps only records above the last one it stored and the node's next output could arrive first. After a reconnect
  the node now re-sends everything central has not acknowledged before any newer output of that run, one catch-up at
  a time, and stops at the first failed send. Output that waits for the catch-up is kept back (up to 1 MiB per run in
  memory, then read again from the node's recording); a run's exit record is kept even when the node's disk failed.

## [0.1.3] - 2026-11-13

Patch release on top of v0.1.2. It makes no production-readiness claim (docs/RELEASE_READINESS.md §17). It contains:
- an incident fix: a node reconnect could trigger a refetch storm in open browser tabs that exhausted server memory,
  which made nodes lose their connection over and over (ADR-0078);
- lean run lists, a live start for the change feed, and a bound on how long a response may wait for its client;
- a security fix: a concurrent login could undo a password change or reset;
- steadier write-transaction-guard tests on slow or busy machines.

**Upgrade notes:**
- **From v0.1.1** (v0.1.2 was withdrawn): everything in the v0.1.2 upgrade notes below applies. In short:
  - Take a backup first. Central migrations 0048/0049 and the agent's journal migrations 0006/0007 are
    forward-only.
  - Node protocol 1.11: upgrade central first, then every node agent.
  - Claude Code's self-update is off on every node: a node whose `claude` stayed current only through it stays on its
    version until `harness.claude.manage: true` is set, or you update it yourself.
- **From v0.1.2:** no new migration and no protocol change; upgrade central and the agents in any order.
- **API changes:**
  - Run lists (`GET /api/v1/spaces/{space_id}/runs`, `spaceman run list`) no longer return `manifest`,
    `launch.prompt` or `launch.system`; pass `include=manifest`, `include=prompt` or both (`--include` in the CLI).
    A single run's GET is unchanged.
  - `GET /api/v1/watch?live=1` starts the change feed at the newest change. Without it the feed still replays from
    the start.
  - A run's `observed_at` is when its node last reported a change to it; a report that changes nothing no longer
    moves it.
- **New setting `http.write_idle_timeout`** (central, default `1m`, from 1s to 10m): each 32 KiB of a response must
  reach the client within it, or the connection is closed. It bounds progress, not the whole response, so slow
  downloads, exports and streams that keep moving are unaffected. WebSocket connections keep their own timeouts.
- **Security:** a password change or reset could be undone by a concurrent login that upgraded the old password's
  hash; that upgrade now applies only while the password is unchanged.

### Added

- Telegram renderer (ADR-0083, TG-5; not usable until the bot ships): `internal/telegram/render` turns Markdown into
  Telegram message entities (UTF-16 offsets, links limited to an allowlist, 4096/1024-character splitting with at most
  four parts) and turns inbound entities back into Markdown. It isn't wired to a sender yet.
- `GET /api/v1/watch?live=1` starts the change feed at the newest change, without replaying the retained history, and
  sends a first `position` event with that change's ID.

### Changed

- **API:** run lists (`GET /api/v1/spaces/{space_id}/runs`, `spaceman run list`) no longer return each run's
  `manifest`, `launch.prompt` or `launch.system`. Those hold the run's resolved prompts, which made a list of 200 runs
  megabytes, and the web UI refetches lists whenever a run changes. Ask for them with `include=manifest`,
  `include=prompt` or both (`--include` in the CLI). Getting a single run still returns everything.
- **API:** a run's `observed_at` is now when its node last reported a *change* to it. A report that changes nothing,
  such as every hello re-reporting the node's runs, no longer moves it.

### Security

- A password change or reset could be undone by a concurrent login that upgraded the old password's hash. The upgrade
  now applies only while the password is unchanged, and a login whose password changed after it was checked fails
  without opening a session.

### Fixed

- Nodes could lose their connection to central every 15 s, while central's memory grew to gigabytes, after central
  restarted with a web UI tab open. Every reconnect re-reports every run in the node's journal. Central stored each
  report as a run change even when nothing had changed, and each change made every open tab refetch that space's runs
  list. Superseded requests were never aborted, so a tab could hold hundreds of them open without reading them. Central
  kept every response, and the memory pressure starved the nodes' connections, which reconnected and started over.
  - A run report that changes nothing now writes nothing and emits no change. A node of 2500 runs reconnects in under
    a second instead of 9 s, with no change entries instead of 2500.
  - The web UI applies change-feed refetches at most once per 250 ms per query, keeps a fetch in flight instead of
    restarting it, and aborts requests nobody waits for any more.
  - Central disconnects a client that stops reading its response: each 32 KiB of a response must reach the client
    within `http.write_idle_timeout` (default 1m). Slow transfers that keep moving are unaffected.
  - Central stops applying a hello once that connection has ended.
- Opening a web UI tab replayed the whole retained change feed and refetched the affected queries once per change:
  with 60 space and 60 human-action changes in the history, opening the inbox made 318 API requests. It now makes 21.
  - A new tab opens the change feed live (`GET /api/v1/watch?live=1`): from the newest change, with a first `position`
    event, instead of replaying the history. It refreshes its open queries once.
  - Cached data is fresh for 60 s instead of 5 s, since the change feed invalidates what changes. Every request the
    UI makes for a query can be aborted.
- Central no longer logs `superfluous response.WriteHeader call` when a handler writes an error after the client
  aborted the response.
- Break-glass recovery with a new administrator, and a login that replaces a password hash made with older
  parameters, no longer run Argon2id while holding the database write lock. They hash first and only store the
  result in the transaction.
- Store-backed tests hold the write lock for much less time on slow or busy machines, so the write-transaction guard
  (ADR-0062 update 2026-11-13, hold analysis) trips less. The guard's limits are unchanged. A seeded test database is flushed to disk before the
  store opens, so its first commit (`keyring.load`) no longer flushes it. Test stores open 2 reader connections
  instead of `max(4, GOMAXPROCS)`. `make race` builds modernc.org's SQLite without `checkptr` instrumentation.

## [0.1.2] - 2026-11-13

Patch release on top of v0.1.1. It makes no production-readiness claim (docs/RELEASE_READINESS.md §16). It contains:
- node-managed Claude Code versions (ADR-0077): a verified per-version cache, launches by absolute path, per-profile
  version pins, per-run versions and the current versions on each node;
- Claude Code's self-update turned off in every run;
- web terminal fixes: no tab freeze on a fresh cache, and no stale glyphs from older output (ADR-0076);
- the New inbox item dialog's scrollbar with writing-assistant extensions;
- Go 1.27.4 (GO-2026-6859);
- steadier CI on hosted runners (ADR-0062 updates).

**Upgrade notes** (from v0.1.1):
- **Take a backup first.** Migrations 0048 (a profile's Claude Code version pin and each run's version) and 0049
  (when a node last reported its capabilities) are forward-only, and so are the node agent's journal migrations 0006
  (each run's Claude Code version and path) and 0007 (an index for the cache's GC). Central also writes a
  pre-migration snapshot; going back means restoring it.
- **Node protocol 1.11.** Upgrade central first, then every node agent. Older agents keep working, but they never get
  a pinned launch (such a run fails as `node_too_old`), and they report their harness versions only when they
  connect.
- **Claude Code's self-update is now off on every node.** Every Claude Code run gets `DISABLE_AUTOUPDATER=1` and
  `DISABLE_UPDATES=1`, managed or not. A node whose `claude` stayed current only through Claude Code's own
  self-update (the copy in `~/.local/bin/claude`) **stays on its current version** until you set
  `harness.claude.manage: true`, or update it with your own tooling. (`harness.claude.self_update: true` restores the
  old behaviour on an unmanaged node.)
- **Managed mode** (`harness.claude.manage: true`, off by default) is configured with the node agent's
  `harness.claude.*` settings: `channel` (`latest`, `stable` or an exact version), `base_url`, `cache_keep` (the newest
  versions kept, default 3), `cache_keep_pinned` (pinned versions kept, default 5), `refresh` and `download_timeout`.
  The cache lives under the agent's data directory and takes about 340 MB per version, so about 1 GB with the
  defaults plus any pinned versions; size the disk for it. A harness profile's `harness_version` pin is honoured: a
  managed node downloads an exact pin when it's missing and never falls back to another version, and an unmanaged
  node runs a pin only when it equals its installed version.
- **The web terminal's WebAssembly module comes from a pinned ghostty-web prerelease** (0.4.0-next.21), which fixes
  stale cells upstream; the JavaScript stays on ghostty-web 0.4.0 until an upstream release has the fix (ADR-0076).
- **Built with Go 1.27.4**, which fixes GO-2026-6859 in `net/http`. If you build from source, use Go 1.27.4 or later.

### Added

- Current harness versions on nodes (SM-HARN-008, ADR-0077, node protocol 1.11, migration 0049). A connected node
  sends its capabilities again (`node.capabilities`) when its Claude Code cache changes or a channel resolves to
  another version, and at least every 30 minutes, detecting every harness again each time, so an unmanaged `claude`
  updated by hand is reported too. Central stores the report from the node's current connection only, and the node
  API gains `capabilities_updated_at`. A harness entry's `version` and `path` mean what a new run would get now. The
  Nodes page opens a node's detail: per harness, managed or detected, the channel, the resolved `latest` and
  `stable` with when they were resolved, the cached versions and the cache size, and when the node last reported,
  with a stale badge when a connected node's report is over an hour old (an offline node is shown as offline).
  Central cleans a bad harness entry in hello or the report (an invalid version is dropped from that entry, with a
  warning) instead of refusing the whole report. Older node agents keep reporting only when they connect. Operator guide:
  "Versions on nodes".
- Per-profile Claude Code version pins and per-run versions (ADR-0077, node protocol 1.11, migration 0048). A
  claude-code harness profile can set **Claude Code version** (`harness_version`): empty follows the node's channel,
  and `latest`, `stable` or an exact `X.Y.Z` override it. On a managed node an exact pin is downloaded when missing and
  never falls back to another version, and the cache keeps pinned versions. On an unmanaged node a pin runs only when
  it equals the installed version (by that absolute path); otherwise the launch fails with "this node does not manage
  Claude Code versions". A node agent below protocol 1.11 never gets a pin: the run fails as `node_too_old` ("node X
  doesn't support pinned Claude Code versions; upgrade it"). An API update that leaves `harness_version` out keeps
  the pin.

  Run reports carry the version a run launched, what it requested and the fallback reason. Central stores them on
  the run (`harness_version`, `harness_requested`, `harness_fallback`), and the API and the run page show them. The
  manifest's `dependencies["claude-code"]` becomes `{requested, node_default}` for protocol 1.11 nodes that manage
  Claude Code; other nodes and other harnesses keep the version string. Report values that aren't a valid request or a
  known fallback reason are dropped. GC keeps at most `harness.claude.cache_keep_pinned` (default 5) pinned versions
  launched in the last 30 days, besides versions in use. A managed node's capabilities add `managed`, `channel`, `resolved`, `cached` and
  `cache_bytes`. Operator guide: "Pinning a version per profile".
- Node-managed Claude Code launches (`harness.claude.manage: true`, ADR-0077): a Claude Code run launches the verified
  cached binary by absolute path, so the herdr-core core's `PATH` no longer decides which `claude` runs. With nothing
  usable cached the run waits in starting for the download, bounded by `download_timeout`. The agent's journal records
  each run's version, path, requested channel and fallback reason (migration 0006), and the cache's GC keeps the
  versions of live and adoptable runs. A launch holds its version against GC from the decision until the journal
  records it. At start the agent cleans interrupted downloads, re-hashes the versions it will use and collects garbage.
  The prefetch loop and periodic GC run in the background. A launch re-hashes a cached binary whose stat fingerprint
  (device, inode, size, mode, ctime) changed since it was last hashed, and reinstalls it on a mismatch. Operator guide:
  "Managed Claude Code".
- Node-managed Claude Code, groundwork (not used by launches yet): the `internal/harness/dist` package resolves
  `latest`, `stable` or an exact version from the release server, downloads it into an immutable per-version cache
  under the agent's data directory after checking the manifest's version, size and SHA-256 (checksum only, no
  signature check yet), keeps the newest few plus pinned and in-use versions, and prefetches channel targets. Cached
  versions are read-only and looked up without following links; a damaged one is reinstalled; a channel never falls
  back to a version newer than its target; redirects stay on the release host; musl is detected from `/bin/sh`. New node
  settings `harness.claude.{manage, channel, base_url, cache_keep, refresh, download_timeout, self_update}`;
  `manage` defaults to off. The cache itself changes nothing on upgrade, but the updater change below does (see the
  upgrade note under "Changed").

### Changed

- **Upgrade note (protocol 1.11, migration 0048):** upgrade central before the node agents. Central accepts older
  agents, but it sends a pinned launch only to a 1.11 agent. Migration 0048 is forward-only (restore the pre-upgrade
  backup to go back).
- Every Claude Code run now gets `DISABLE_AUTOUPDATER=1` and `DISABLE_UPDATES=1`, on unmanaged nodes too, so runs no
  longer update themselves into the agent's `HOME`. `harness.claude.self_update: true` restores self-updates on an
  unmanaged node; it is refused together with `manage: true`. The node warns about a profile's updater settings only
  when it turns the updater off.

  **Upgrade note:** a node whose `claude` stayed current only through Claude Code's own self-update (the copy in
  `~/.local/bin/claude`) now **stays on its current version**. Nothing updates it any more until you do one of these:
  set `harness.claude.manage: true`, update it with your own tooling, or set `harness.claude.self_update: true`.

- CI's and the release workflow's coverage step uses a 1 s write-transaction guard on GitHub's hosted runners, the
  same as the race tests, instead of 250 ms (ADR-0062 update). Shared runners held the lock 260–750 ms in packages a
  pull request didn't touch. Local runs keep 100 ms and `SM_TEST_TX_MAX=500ms make check` keeps 500 ms.
- CI's and the release workflow's login-timing and fuzz jobs use a 250 ms write-transaction guard on GitHub's hosted
  runners, instead of the 100 ms default (ADR-0062). The login-timing job failed on it
  while its timing check passed. Local runs keep 100 ms.

### Fixed

- Managed Claude Code launches: a stop (or a fenced lease) while a launch waits for the download is no longer undone
  when the download completes; the run stays stopped and never starts. The version is held after the clone,
  worktree and memory preparation, right before the launch is built. That hold takes it from the cache only and never
  waits under the space's lock: when the version waited for is gone by then, the launch waits again (at most three
  times, within `download_timeout`), so a download never blocks the space's other launches. A startup or launch check interrupted by a
  cancelled or expired context no longer removes a good cached version.

- Web UI: the New inbox item dialog (and any other dialog with the Markdown editor) no longer shows a vertical scrollbar
  when a writing-assistant browser extension such as Grammarly is active. The extension's overlays, absolutely
  positioned next to the editor, made the dialog scroll by however far they reached below it. The editor now contains
  them (`contain: layout`). Long dialogs still scroll.

- Web terminals no longer show older output through new lines (in Claude Code runs, characters of earlier lines
  appeared between the words of new ones; ADR-0076 update). ghostty-web 0.4.0's WebAssembly module left old
  cells in rows that scrolled in after a style reset (upstream ghostty-web #180). The terminal now loads the module
  of ghostty-web 0.4.0-next.21, which has the fix, and keeps 0.4.0's JavaScript until an upstream release with the
  fix is adopted. `web/e2e/terminal-stale.spec.ts` covers it.
- Opening Terminals or Spaces no longer freezes the tab, which took about a minute on a desktop and minutes on a
  phone (ADR-0076). Tracing a fresh-cache load with CPU throttling showed where the time went:
  - A terminal applied its run's stored output record by record, and a replay applied all of it in one go. Both now
    apply it in short slices, showing "Loading output…" and drawing once the terminal has caught up.
  - ghostty-web 0.4.0 parsed the whole screen once for every row it drew, and drew every blank cell. Both are now
    worked around on our side.

  With 5 MB of output at 4× throttling, the longest task while a terminal loads is now 0.13 s (was 0.9 s live and
  1.5 s replayed). The terminal's WebAssembly module is a cacheable asset compiled once per page, so the CSP's
  `connect-src` no longer allows `data:` (ADR-0020 update). Every terminal still runs in an instance and memory of its
  own. `web/e2e/terminal-load.spec.ts` covers it. ghostty-web's unused inlined copy of the module is stripped from
  the build, so the terminal's chunk drops from 687 kB to 122 kB. The workarounds apply only to ghostty-web 0.4.0,
  the version they were checked against. Seeking a replay while it plays no longer lets a pending playback step write
  into the freshly reset screen.

### Security

- Build with Go 1.27.4, which fixes GO-2026-6859 (an HTTP/1 server crash caused by a `MaxBytesReader` header race
  in `net/http`). `go.mod`, `tools/go.mod` and the `golang` build image in `Dockerfile` and `Dockerfile.agent` now pin
  1.27.4.

## [0.1.1] - 2026-11-09

Patch release on top of v0.1.0. It makes no production-readiness claim (docs/RELEASE_READINESS.md §15). It contains:
- concurrency limits that count live runs only, and a stale-run sweeper that ends runs whose process is gone
  (ADR-0074);
- a configurable terminal size per run (ADR-0075);
- attachments of any size reaching agents (ADR-0073);
- `github-pull-request` version 2, which ignores your own agent's pushes and edits (ADR-0072);
- Harbor Pilot 0.1.4 support;
- web fixes: switching terminals, scrollbars, and pages that failed to load after an upgrade;
- faster and steadier gates and CI (ADR-0069 to ADR-0071).

The real Claude Code suites last ran against 2.1.317 (v0.1.0). This release's new live attachment test ran against
2.1.319.

**Upgrade notes** (from v0.1.0):
- **Take a backup first.** Migrations 0046 (the disconnect grace stored on each lease) and 0047 (a run's terminal
  size) are forward-only. Central also writes a pre-migration snapshot; going back means restoring it.
- **Node protocol 1.10.** Upgrade central first, then every node agent. Older agents keep working, but their runs
  can't be resized: the web disables the size menu and the API answers 409.
- **`nodes.lease_ttl` must now be at most 10m.** Central refuses to start with a longer value, so check your
  configuration (and `SPACEMAN_NODES_LEASE_TTL`) before upgrading. The default, 45s, is unaffected.
- **The first deploy sweeps historic stale runs.** Once the nodes have reconnected and the grace has passed (or 15
  minutes after a lost lease), the stale-run sweeper ends every old run that central still counted as `running` or
  `unknown` but whose process is gone. Each one is moved to `lost`, has its credentials and gateway tokens revoked and
  its desired state set to `stopped`, and is reported as ended. For a worker run, this tells the space's control that
  the run ended and names its task, so **control may re-queue or relaunch old tasks**. After the upgrade, look at the
  `run.reconciled_stale` audit events and close the tasks you no longer want. Runs that had already ended but kept
  `desired = running` stop counting against limits at once, so spaces that were refused may admit new workers right
  away.
- For contributors: `make e2e` takes about 13 minutes longer, because the new UI overflow guard
  (`e2e/ui-overflow.spec.ts`) opens every page, modal and drawer at three widths in both themes.

### Added

- `make check-fast`, the inner-loop gate (ADR-0069): vet, the custom analyzers, golangci-lint, traceability, the
  analyzer tests and every Go test without the race detector, from go test's cache. With nothing changed it takes
  seconds. `sqlc-check` and `api-check` run only when their inputs changed since the merge base
  (`scripts/changed.sh`). AGENTS.md has a new "Gates" section on which check to run when.
- **`github-pull-request` version 2** (ADR-0072): `exclude_author` ignores events the space's own agent account caused,
  and `ignore_updates=true` ignores pushes (`synchronize`) and edits (`edited`), which were most of the pull request
  items that woke control. `repository` is optional and `owner` is new, as in the ADR-0061 templates, and the pull
  request title moves from the item title to the body. The site guide (Events) has the steps for switching a
  subscription, and a custom template that does the same on v0.1.0.
- Built-in event templates can gain new versions on existing installations: central publishes a built-in's missing
  versions at start, never over an administrator's own version of it. Subscriptions that follow the latest version
  move to the new one as on Publish; pinned subscriptions and version 1 are untouched.
- Harbor Pilot 0.1.4 support (recommended; 0.1.3 stays the minimum). The connection page shows the gateway's version
  (`get-harbor-pilot-gateway`, `GET /api/v1/harbor-pilot/gateway`, `spaceman harbor-pilot version`), read from Harbor
  Pilot's `GET /api/v1/system` with the service-principal token; an older gateway shows as "older than 0.1.4". On
  0.1.4 or later, budget status is read for the space's project and client binding only
  (`budgets/status?project_id=…&binding_id=…`), and Harbor Pilot resolves the project's workspace, so SpaceMan no
  longer reads every budget nor looks projects up. Older gateways keep the previous path, and so does a filtered read
  that answers 404 (the project or binding is gone in Harbor Pilot) or that holds budgets a 0.1.4 filter never returns
  (a gateway downgraded while its version was cached): such an answer drops the cached version. A launch's budget
  check waits at most 15 s for Harbor Pilot in all, not 15 s per read.
- The Limits card shows one-time budgets (Harbor Pilot 0.1.4 `period: once`): "one time", **Never** or when the window
  ends, **Scheduled** and **Ended** badges, and what is left of each budget.
- Web: the size of a run's terminal is configurable (ADR-0075, SM-UI-007).
  - A size menu on the live terminal offers Fit to window, 80×24, 120×40, 160×50, 200×60, a custom size (40–400
    columns, 10–200 rows) and Node default. The node resizes the program's terminal, so the agent redraws for it.
  - The size belongs to the run: every viewer sees it, scaled to their window. Resizing needs `terminal:takeover`;
    while the terminal is taken over, only the holder can resize it.
  - Each change is audited (`run.terminal_resized`).
  - The browser remembers your choice but never applies it on its own.
  - Recordings note every size change, so replays redraw at the right size.
  - API: `PUT` and `DELETE /api/v1/spaces/{space}/runs/{run}/terminal-size`; CLI: `spaceman run terminal-size set|clear`.
- Space limits `terminal_cols` and `terminal_rows` set the terminal size new runs start at (0, the default, keeps the
  node's `runner.terminal_size`). Changing them needs `terminal:takeover`, like resizing a running run.

### Changed

- The wall-clock comparison of failed logins for known and unknown accounts (Hallpass HP-AUTH-005) moved out of
  `make test`, `race`, `coverage` and `check` into `make test-login-timing`, which CI runs as its own job and the
  release workflow as a gate before building binaries and images (ADR-0071).
  Under load the Argon2id check's own run time varied by more than the 25% band, so the comparison failed on
  unchanged code. The deterministic check (one verification against a same-parameter dummy hash) still runs in every
  suite.
- Faster `make check` (ADR-0069):
  - the coverage floor tests its packages in one `go test -cover` run instead of one run per package (about 150 s
    down to under 30 s on a 32-core machine); a failing test, a package below the floor and a package with no
    coverage line still fail. `COVERAGE_PARALLEL` sets how many packages test at once (default: a quarter of the
    CPUs, 1 to 8; CI uses 1, as before, so the write-transaction guard keeps its calibration);
  - `make check` runs its steps fail-fast: vet, custom vet, lint, traceability, analyzer tests, the coverage floor,
    then the race tests.
- CI no longer downloads sqlc or golangci-lint from GitHub release assets on a cache hit (ADR-0069). `bin/sqlc` is
  cached under the hash of `scripts/install-sqlc.sh` (pinned version and checksums), and its download retries
  transient errors. golangci-lint is built with `go install` through the Go module proxy and cached per version and Go
  toolchain.
- Local `make race` (and so `make check`) uses go test's cache and reruns only what changed (ADR-0070). CI and the
  release workflow run `make race RACE_COUNT=1`, so every test still runs there.
- `make race` runs `internal/app`, its critical path, as its own `go test -race` invocation alongside the other
  packages, with its own `-parallel` cap `RACE_APP_PARALLEL` (default `min(16, nproc/2)`, at least 4; CI keeps 4)
  (ADR-0069, `scripts/race.sh`). On 16 or more CPUs its serial tests run in a second process at the same time
  (`RACE_APP_SPLIT`).
- The Makefile's browser targets take `SM_CHROMIUM_ARGS` from the environment (nothing when it is unset, as in CI), and
  the site targets add `--single-process` to a non-empty value.
- `make lint` waits for another golangci-lint run on the machine (`--allow-serial-runners`) instead of failing with
  "parallel golangci-lint is running", so `make check` runs started together in different worktrees both get through.
- The heavy gates (`race`, `coverage`, `site-a11y`, `e2e`, `e2e-browsers`, `screenshots`) take a slot of a node-wide
  `flock` semaphore first, so several runs on one machine take turns instead of contending (ADR-0069,
  `scripts/gate-slot.sh`). `SM_GATE_SLOTS` (default 2 on 16 or more CPUs, otherwise 1; `0` disables it, and it is off
  when `CI=true`) and `SM_GATE_LOCK_DIR` configure it; `make check` holds one slot for coverage and race together.
- The site's axe audit runs in parallel shards (`SM_A11Y_SHARDS`, by default a quarter of the CPUs, at most 8) and
  waits for VitePress to be ready instead of for network idle: about 1.5 minutes instead of nearly 10 on a 32-core
  machine. A local `make site-check` skips the audit when nothing the site is built from changed
  (`scripts/site-affected.sh`, the rule CI already used; `SM_SITE_A11Y=1` forces it).

### Fixed

- A failed login for an unknown account now does the same two database reads as one for a known account (the
  credential lookup runs with an ID no user has), so the work before the password check no longer depends on whether
  the account exists (Hallpass HP-AUTH-005, ADR-0071). `TestLoginReadsSameForUnknownAccounts` checks that both
  paths run the same reads, using the new test-only `storetest.WithReadObserver`.
- The test guard for short write transactions (Hallpass HP-DB-004) measures how long a transaction holds the write
  lock, no longer the time it waited for the writer or retried a busy database (ADR-0070). Under load, short
  transactions that queued behind another writer failed tests on unchanged code. It takes the longest hold of a
  transaction's attempts, so an attempt that held the lock for long before a busy rollback still counts, and it now
  checks failed transactions too (not those whose context ended). The guard's limits are unchanged;
  `spaceman_store_tx_duration_seconds` still reports the end-to-end duration.
- **A launch whose delivery failed once is delivered again on its own.** When central could not prepare a command for
  a node for a transient reason (a failed read, the run's secrets not resolved), the command waited until something else
  woke the node's dispatch, which might never happen, and the run stayed pending. The dispatch now retries with a
  backoff (250 ms up to 30 s). A run's command whose stored payload doesn't decode, which no retry can fix, fails the
  run as `invalid_command` instead.
- **A response held for an agent instance goes to the instance's next run when it launches.** Before, it was pinned
  only when the deadline loop happened to run after the launch (the run reporting idle, another response, the hold
  ending up to an hour later).
- **Human-action deadlines retry after a failed round, with a backoff.** A round in which a job failed (routing a
  stranded response, an expiry) is tried again after a second, backing off to a minute, instead of at the next
  deadline, which could be an hour away. A job that kept failing while its deadline was due made the loop run rounds
  back to back; it now waits for the backoff (other deadlines wait for it too, or for a change that wakes the loop).
- **Role memory's git housekeeping finishes inside the call.** An automatic `git gc` or `git maintenance` started by a
  memory fetch or commit ran detached and kept writing into the repository after the call returned.
- Tests: `eventually` in the end-to-end tests prints the runs, leases, node commands, human notifications and
  terminals when a wait times out.
- Attachments over 512 KiB (screenshots, for example) now reach agents whole (ADR-0073):
  - The new `spaceman_attachment_save` tool, served by the node's MCP bridge, saves an attachment of any size as a
    file in the run's own folder, with its size and SHA-256 checked. The agent then opens it with its own file tools:
    Claude Code's Read tool shows images, with no permission dialog.
  - The file name is sanitized, the file is `0600`, one save is at most 50 MiB, and the folder is removed when the
    run ends.
  - `spaceman_attachment_get` no longer cuts a file silently. Every result says `truncated`, `offset`,
    `returned_bytes` and `next_offset`. A large binary file is no longer returned as partial base64, and a large text
    file comes in chunks. New `offset`/`length` arguments read a range.
  - The node never resolves a folder the run can write by path on the way to the attachments folder, so a run that
    swaps a folder for a link can't redirect a save or a cleanup (security review M1). Saved files of runs found lost
    after a node restart are removed too, a save that outlives its run leaves no file, and a ranged read of text
    always returns bytes before the end of the file (`next_offset` is present whenever `truncated` is).
  - Takes a node agent on this release for the save tool. Central and agent can be upgraded in any order; there is no
    protocol change.
- Harbor Pilot 0.1.4 budget status: a one-time budget without an end (`period_end: null`) no longer shows a reset
  date in year 1 on the Limits card or in a launch refusal; a used-up one-time budget's refusal says it never resets
  (or when its window ends). Scheduled, expired, disabled and unknown budget states never refuse a launch (they
  didn't before either) and no longer show as raw states. The project usage window ignores budgets that aren't
  counting yet or any more.
- The space concurrency limit and the node capacity count only live runs, the same runs `spaceman_runs_list` shows.
  Runs that exited, failed or were lost (for example after a node agent restart) no longer hold their slot until an
  operator stops them. **On upgrade**, runs that already ended but still had `desired = running` stop counting at
  once. The refusal names the counted runs (ID, kind, role, state) and says which runs don't count: control, KB
  support and dreamer runs (ADR-0074).
- A stale-run sweeper moves runs that central still counted as live but whose process is gone to `lost`, freeing
  their slots. These are runs that were live when a node reconnected and that its hello did not report
  (`stale_not_on_node`, two minutes after the connection, measured on a monotonic clock), and runs of a node that stayed
  disconnected past its lease, the larger of the old and current disconnect grace, and a 15-minute margin
  (`stale_lease_lost`). It never sweeps a pending launch, a run with an unacknowledged launch command, a run no node has
  reported, or any run of a connection whose hello reports did not all apply, and it re-checks its signal in each run's
  end transaction. A swept run's undelivered commands are cancelled and its desired state becomes `stopped`. It
  records an audit event `run.reconciled_stale` and the metric `spaceman_runs_reconciled_stale_total{reason}`
  (ADR-0074). Migration 0046 stores the disconnect grace on each lease.
  - **A false positive is killed.** The sweeper sends no stop itself, but a swept run's later "running" report is
    fenced with a `stop kill`. The lease rule assumes synchronised clocks: run NTP on central and the nodes.
  - **On the first deploy**, the sweeper ends every historic run left `running` or `unknown` that meets the rules, at
    once: each publishes `run_ended`, has its credentials revoked and notifies the space's control, which may re-queue
    or relaunch the run's old task. Review the `run.reconciled_stale` audit events and close tasks that are no longer
    wanted. A swept control run is not restarted automatically.
- A run that ended on its own (it keeps `desired = running`) no longer blocks archiving its space or relocating it
  without `stop_runs`, and isn't counted among the space's "active runs" (ADR-0074).
- When a node reconnected while its old connection was still open, the old connection's end recorded a disconnect and
  marked the node's runs `unknown` although the node was connected again. A replaced connection's end now records
  nothing.
- The stale-run sweeper doesn't use a revoked lease for `stale_lease_lost`, since the node may hold it up to
  `nodes.lease_ttl` longer than central records. `nodes.lease_ttl` must now be at most 10 minutes (ADR-0074).
- Web: switching between run terminals no longer shows lines of a terminal viewed earlier inside the new one, and no
  longer grows the page's memory until the tab freezes on "Connecting…" and crashes. ghostty-web 0.4.0 ran every
  terminal in one shared WebAssembly memory that a freed terminal leaves uncleared; each terminal now runs in an
  instance of its own, released with it. The first terminal of a page also waits briefly for its font.
- Terminal stream: a viewer that fell behind once no longer re-reads the whole recording from storage for every later
  record; it catches up once and then streams live again. This also fixes a data race on the viewer's lagged flag.
- Web UI: no unwanted scrollbars. Dialogs scroll vertically only and wrap long IDs, keys and URLs, so they no longer
  show a horizontal scrollbar (and with it a vertical one) for a few stray pixels. The space sections row no longer
  shows a vertical scrollbar and fits 1280 px screens. On phones, the inbox, an inbox item and the human actions list
  no longer scroll sideways, and message subjects and human action titles keep a readable width. Wide tables in the
  role configuration and type permissions dialogs scroll in their own box. A new end-to-end check
  (`e2e/ui-overflow.spec.ts`) opens every page, modal and drawer at 390, 1280 and 1920 px in both themes with real
  scrollbars.
- Web: opening a page (Spaces, Terminals, Needs you, …) in a tab that was open across an upgrade, or after a dropped
  connection, no longer ends in "Something went wrong!" until a manual refresh. A part of the app that fails to
  download (`import()` or Vite's stylesheet preload) reloads the page once to pick up the current build, guarded
  against a reload loop; until the reload lands only a loader shows (no error screen flashes first), and if it still
  fails the page says "SpaceMan was updated" with a Reload button. A chunk that downloaded but threw while it ran is
  shown as the error it is, not reloaded. Other route errors show their message with Reload and Try again. A missing `/assets/*` file is a 404 with
  `Cache-Control: no-store` (it carried the one-year `immutable` header before), and an `/assets/` directory is no
  longer listed.

### Notes

- After upgrading Harbor Pilot to 0.1.4, budgets that already warned or were used up in the current period send their
  `budget.threshold.*` event once more (Harbor Pilot's upgrade note), and raising a limit re-arms them. Each is a new
  event, so the `harbor-pilot-budget` template makes one more inbox item, in the same correlation as the first; a
  redelivered event is still a duplicate. Nothing else repeats.

## [0.1.0] - 2026-11-08

First release. It is ready for evaluation by trusted teams; it makes no production-readiness claim
(docs/RELEASE_READINESS.md §14 lists what was verified, the known issues it ships with and the accepted risks).
Since v0.1.0-rc.9: the Harbor Pilot gateway (ADR-0060); the SpaceMan admin MCP for control and worker runs and the
external `/api/v1/mcp` endpoint for your own MCP client (ADR-0063); the System page (ADR-0067); scheduled reminders
for control (ADR-0065); no terminal questions or plan mode in Claude Code runs (ADR-0066); a deny-only hook in place
of permission dialogs, with every space's instance files protected and auto memory moved out of the space folder
(ADR-0068); Claude Code's classic renderer so watchers can scroll back (ADR-0064); and fixes, among them opening and
closing the database under a brief lock. Tested with Claude Code 2.1.317.

**Upgrade notes** (from v0.1.0-rc.9):
- **Take a backup first.** Migrations 0040, 0041, 0043, 0044 and 0045 are forward-only (0042 is retired and never
  used). Central also writes a pre-migration snapshot; going back means restoring it.
- **Node protocol 1.9.** Upgrade central first, then every node agent. Older agents keep working, but the node-side
  changes below need agents on this release, and a Harbor Pilot gateway launch to an older agent fails as
  `agent_too_old`.
- **New node behaviour in Claude Code runs:**
  - each instance's auto memory moves to `<data_dir>/automemory/<space>/<instance>/…`; the memory an instance kept in
    its config dir is copied there at its first launch on this release (the old copy stays);
  - deny rules protect every space's instance files (settings, instructions, plugins, hooks, auto memory and
    transcripts) from a run's file tools;
  - a `PermissionRequest` hook denies every permission dialog, so an `ask` rule now works like a deny;
  - a profile's own `autoMemoryDirectory` is ignored (the node logs a warning);
  - stopping central drains for at most 8 s for the database, inside the units' `TimeoutStopSec=45s`.
- **Check your profiles:** remove any `additionalDirectories` entry that grants the node's whole `spaces/` folder, and
  any dialog-denying hook script. This release adds the run's own space folder and the hook automatically.
- `/api/v1/mcp` is on for every user API token; leave it out of your reverse proxy if you don't want it.
- **Codex users:** if a node ran Codex with an API-key account on an earlier release, rotate that key (see Fixed).

### Added
- **System page and API** (ADR-0067; SM-OBS-006). Administrators see this instance's status on a new **System** page
  (under Access), with `GET /api/v1/system` (`get-system`, `system:read`) and `spaceman system status`: readiness,
  whether the database accepts writes, version, commit and build date, a boot ID that changes at every start, start
  time and uptime, the database schema version, the node protocol range, the database and data-disk sizes, and counts
  of spaces, connected nodes, running runs, open human actions and live streams. It names no key material, key path
  or token. The version alone stays public (`get-system-info`) and is now shown in the account menu. The admin MCP
  offers `get-system` as a read. A counts or node-state source that fails shows as `unavailable` (`counts.error`,
  `counts.nodes_error`) instead of failing the response; build, uptime and storage are still returned. Central logs the
  boot ID in its `spaceman serving` line. **Migration 0045** (forward-only) adds a partial index over live runs, so
  the running-runs count doesn't scan every run ever recorded. No node protocol change.
- **SpaceMan admin MCP** (ADR-0063, Accepted; SM-MCP-005…009; AM-1 to AM-10). SpaceMan's own API as MCP tools: for
  a space's control and worker runs through a `spaceman` gateway server, and for your own MCP client through the
  external endpoint. An agent in an "infra" space can then administer SpaceMan with an administrator's authority, short
  of hard exclusions, and with dangerous calls approved by a person. **Upgrade notes:**
  - **Migration 0044** (forward-only) adds `mcp_servers.kind`, `api_token_id` and `spaceman_json`, and
    `api_tokens.purpose`. Existing servers become kind `http`; nothing else changes until you add a SpaceMan server.
  - **New operations:** `create-spaceman-mcp` (`POST /mcp-servers/spaceman`), `update-spaceman-mcp`
    (`PUT /mcp-servers/{id}/spaceman`) and `reissue-spaceman-mcp` (`POST /mcp-servers/{id}/reissue`), all
    `mcp:write` with a step-up; `list-spaceman-mcp-tools` (`GET /mcp-servers/spaceman/tools`) previews a preset's
    tools. MCP servers and a run manifest's `mcp_servers` entries now carry `kind`. CLI: `spaceman mcp spaceman
    create`, `update`, `reissue` and `tools`.
  - **External endpoint:** `/api/v1/mcp` is new and on by default for every user API token, with no setting to turn
    it off (see below). If your reverse proxy forwards all of `/api/v1/`, it is reachable wherever the API is; leave
    `/api/v1/mcp` out of the proxy if you don't want it.
  - **Terraform:** the `spaceman_admin_mcp_server` resource is in terraform-provider-spaceman PR #12 (not yet
    released); grant it with the existing `spaceman_mcp_grant`.
  - No node protocol change: agents need no update.
  - **Approvers:** the root token may approve the external endpoint's dangerous calls (the path for a
    single-administrator installation). An in-run call's approval is decided in the run's own space, also when the
    call acts on another space.

  **Identity:** each SpaceMan server has an internal API token, minted with it, owned by its creator, scoped like any
  API token (covered by the owner's grants, and by the creating token's, whose expiry it never outlives), and without
  a secret: it is never accepted as a bearer token and nothing reaches nodes, runs, manifests or Terraform state.
  Re-issue revokes it and mints a new one in one step (grants, owner, expiry); deleting the server revokes it.
  Revoking the token, disabling the owner or the server, or revoking the grant stops the tools on the next call, with
  the reason. Calls are audited as the owner, `via spaceman_mcp:<token>`, with the agent run.

  **Tools:** one tool per API operation, run in process through the API's own handler chain (authorization,
  validation, idempotency, audit). Every operation has a reviewed class (read, write, dangerous, sensitive-read,
  excluded), enforced by a test. Presets: `admin` (the default, "infra": read, write and dangerous tools on every
  space), `operate`, `read` and `custom`. Hard exclusions: sign-in and step-up operations, tokens, approvals and
  human-action answers, takeover and terminal input, one-time secrets, sign-in providers, SpaceMan MCP management and
  restore acknowledgement. Retried creates are idempotent, API problems are tool errors and results are capped. Only
  control and worker runs see the tools.

  **Approvals:** dangerous tools always need a person's approval (operation `mcp.<slug>.<class>.<tool>`, one use for
  the exact arguments unless a stored policy says otherwise); `approval: writes` gates writes too. The API checks a
  launch's kind as resolved, so a space role of diagnostic kind can't bypass the gate, and saving a space role is
  dangerous; launching a run is a write, also with a permission mode (up to the ceiling) or account override. An agent can't manage SpaceMan MCP servers, answer human actions or decide approvals through the tools.

  **Web:** **Add SpaceMan MCP** on the MCP servers page and on a space's new **MCP servers** card. The dialog defaults
  to the slug `spaceman-admin` (`spaceman` is reserved), the control role, "Administer everything (infra)" and no
  expiry; scopes beyond your own grants are disabled; tools come from a preset or a custom pick grouped by area, with
  dangerous tools flagged. The server's drawer shows the owner, the token and the effective tools, and confirms
  Re-issue, Disable and Delete. The API tokens page labels internal tokens.

  **External endpoint** (SM-MCP-009): `/api/v1/mcp` serves the same tools over streamable HTTP to your own MCP client,
  with your API token as a bearer and exactly that token's grants. Browser requests, session cookies, the root token
  and internal tokens are refused, and each token is rate limited; each request carries one JSON-RPC message (batches
  and protocol versions before 2025-06-18 are refused). Dangerous calls and diagnostic launches need an approval from
  someone other than the token's user, who may only deny or cancel their own client's request; the root token may
  approve them. Calls are audited via
  the token on the `mcp_endpoint` channel. See the guide's "Your own MCP client" page.

  Verified end to end in process (`TestSpacemanMCPOverBridge`: a control run's harness MCP client over the real
  `mcp-bridge`, node connection and gateway; `TestMCPEndpoint` over real HTTP). Not yet verified with a real harness
  driving the tools.
- **Harbor Pilot gateway** (ADR-0060, Accepted; SM-GW-001…010). A space can route its runs' model traffic through a
  self-hosted [Harbor Pilot](https://aurimasniekis.github.io/go-harbor-pilot/) gateway. Each run gets its own
  short-lived Harbor Pilot token instead of an account, and the token is revoked when the run ends. Harbor Pilot's hard
  budgets refuse launches early and show on the Limits page, and its events become inbox items. **Upgrade notes:**
  - Needs **Harbor Pilot 0.1.3** or later.
  - Migrations 0040 and 0043 are forward-only (0042 is not used).
  - **Node protocol minor 9:** update every node of a space before opting the space in. A gateway launch to an older
    agent fails as `agent_too_old` before a token is minted. Spaces that don't opt in need no agent update.
  - Terraform support is in terraform-provider-spaceman's next release.
  - There is **no fallback to a direct account** (Q1): while Harbor Pilot is down, a space's routed harnesses can't
    run. Revoking the parent token in Harbor Pilot is the emergency stop for every gateway run at once.
  - Verified against `harbor-pilot serve --demo` 0.1.3 with synthetic upstreams only, with real Claude Code 2.1.317
    and codex-cli 0.166.0 runs through SpaceMan. Not yet verified with a real model provider (evidence map:
    RELEASE_READINESS §13).

  **Connection** (Integrations → Harbor Pilot, `/api/v1/harbor-pilot/connection`, account:write; a change or removal
  needs a step-up):
  - An administrator enters a gateway URL, an optional URL runs use, a parent token with `tokens:mint` and an optional
    service-principal token (`usage.read`, `config.read`).
  - Both tokens are sealed and write-only. Answers, the audit log and the change feed show only whether each is set,
    with a version counter.
  - Stored tokens go only to the address they were entered for. Changing either URL, or newly allowing plain HTTP,
    needs the tokens again in the same request (422 before any call).
  - Central's client needs https except on loopback and follows no redirects. It uses no proxy, and it never dials
    link-local (cloud metadata included), "this network", unspecified, multicast or broadcast addresses, also not
    through NAT64 or 6to4.
  - Every save is checked with Harbor Pilot: a probe token is minted and revoked at once ("parent token lacks
    tokens:mint" otherwise). The check finds the client binding and the parent's expiry. A gateway older than 0.1.3 is
    refused.
  - The connection can't be removed while a space uses it or tokens are live or waiting to be revoked.

  **Space settings** (the space overview, `/api/v1/spaces/{id}/harbor-pilot`, space_config:write):
  - The Harbor Pilot project, checked with Harbor Pilot. Setting or changing it needs account:write, since it decides
    who pays.
  - The harnesses routed through it: any harness, Claude Code and Codex by default.
  - The run-token lifetime: 24 h by default, from 15 minutes to 7 days.
  - Optional allowed models.
  - What a launch does when budget status can't be read: proceed (the default) or refuse.

  **Run tokens:**
  - A routed harness uses no account. A launch that names one is refused, and so is a model outside the allowed list.
    The run's manifest records the gateway and the project.
  - Each run gets its own child token when its launch is dispatched. It can invoke and list models only, and it is
    bound to the space, run, instance, session, project and task. For a fresh Claude Code run, central chooses the
    session ID. A re-sent launch reuses the token, and a token that expires within 5 minutes is replaced.
  - A mint Harbor Pilot refuses fails the run as `gateway_unavailable` with the reason. Network errors, 5xx and 429
    are retried three times first.
  - A slow mint holds up a node's other commands for at most 5 s per new gateway launch: the dispatch pass that
    starts the mint waits up to `GatewayMintWait` (5 s), then the mint goes on in the background and later passes
    don't wait for it.
  - Every end path queues the token's revocation in its own transaction: a reported end, an undispatched launch, a
    stop or pause, a relocation, a purge, and runs a node left unknown past its lease. Central revokes them on its
    deadline timer, retrying until the token expires, and the queue survives a restart.
  - Replacing the parent first revokes the old parent's live run tokens. A revocation Harbor Pilot doesn't confirm
    from the minting parent is recorded as failed, audited, shown on the connection, and counted in the new
    `spaceman_gateway_revocations_failed` gauge.

  **Delivery to the harness** (the node):
  - The token reaches the harness only through the launcher's secrets file, never the terminal's saved state, the
    node's records or argv, and no other credential comes with it.
  - The node checks the run-facing address itself: https unless plain HTTP is allowed, no loopback exemption, a host
    and a valid port.
  - Claude Code gets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN`. A gateway run fails with a reason when its
    profile's Claude Code settings would add a credential, an endpoint, a proxy or a TLS override: `apiKeyHelper`,
    `proxyAuthHelper`, cloud credential helpers, `forceLoginMethod`, or `env` entries such as `ANTHROPIC_*`, every
    `CLAUDE_CODE_*` outside a short list of output and UI switches, `HTTP(S)_PROXY`, `ALL_PROXY`,
    `NODE_TLS_REJECT_UNAUTHORIZED`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `SSL_CERT_DIR` and `NODE_OPTIONS`. Set the
    model on the role. Claude Code's default model works through the gateway: Harbor Pilot accepts its `system`-role
    messages since 0.1.2.
  - Codex gets a `harbor-pilot` model provider pointed at the gateway's Responses API, with the token in
    `HARBOR_PILOT_TOKEN`. It needs a model the Harbor Pilot plan routes, so set the role's model.
  - A stored Claude login and any Codex `auth.json` in the instance are removed before the run starts.

  **Budgets and usage** (needs the service-principal token):
  - A space's Limits page gets a **Harbor Pilot budgets** card. It lists the budgets that cover the space (its
    project, the project's workspace, the client binding, global), each labelled as a hard limit or warn only, with
    its spend, state and reset time.
  - The card is in one of four states: within budgets, warning, budget used up, or status unavailable. It shows an
    error when status can't be loaded. It also shows the project's usage in the current period, labelled **all
    clients**.
  - The API is `GET /spaces/{id}/harbor-pilot/status`, and the CLI is `spaceman harbor-pilot status`.
  - A launch is refused at once when a covering hard-limit budget is used up, naming the budget, its spend and its
    reset time. Control's automatic start raises "Control could not start" with that reason. Warn-only budgets never
    refuse.
  - When status can't be read, the space's setting decides. That includes SpaceMan's own failures, which are logged
    and shown with a fixed reason. The mint also asks Harbor Pilot to check budgets (`check_budget`).
  - After a gateway run ends, central reads back what Harbor Pilot recorded for it, about 15 s, 2 min and 10 min
    after the end. The figures are shown as gateway-observed. They become the run's usage (source `harbor-pilot`)
    only when the harness reported none, as for Codex.
  - Usage is never written as zero, not even for an empty read: the run stays unknown.

  **Events:**
  - New built-in templates: `harbor-pilot-budget` (a project's budgets to its space), `harbor-pilot-quota` and
    `harbor-pilot-access-blocked` (`budget_only` keeps budget refusals only).
  - Titles and correlation keys hold only Harbor Pilot's ids, names, enums and amounts, checked by each template's
    input schema. Free text appears only in the body and the raw payload.
  - Events without a project are routed nowhere automatically: a space gets them only by subscribing (Q4 is open).
- Control can schedule a reminder to itself (ADR-0065). `spaceman_message_send` to `control` takes `deliver_at` (RFC
  3339) or `delay` (e.g. `6h`), from 1 minute to 30 days ahead.
  - The message stays hidden and doesn't count as work until it's due. Then it wakes control, or starts it within the
    `control_autostart` limits, and a control started later is told about it once.
  - Pending reminders: up to 50 per space, and a `dedup_key` returns the pending one.
  - Control lists them with `spaceman_message_scheduled` and cancels with `spaceman_message_cancel`. Operators use
    the Messages page's **Scheduled** view, `GET …/messages?scheduled=true` and `POST …/messages/{id}/cancel`, or
    `spaceman message list --scheduled` and `spaceman message cancel`.
  - Scheduling and cancelling are audited. Migration 0041 adds `messages.deliver_at`.

### Changed
- **No more permission dialogs in Claude Code runs** (ADR-0068, security-relevant). Every run SpaceMan launches, of
  every kind, gets a `PermissionRequest` hook (`spaceman-agent hook permission-request`, part of the node agent) that
  denies every permission dialog Claude Code would show and tells the model to ask through `spaceman_human_ask`
  instead. Even in auto mode, Claude Code shows a dialog for `ask` rules, for reads outside the working directories,
  and when its classifier falls back after repeated blocks, and nobody answers it in an unattended run.
  `skipAutoPermissionPrompt` doesn't prevent these dialogs: it only skips the notice shown when auto mode starts. The
  hook can only deny, never allow, so an `ask` rule now works like a deny. Each run also gets its own space folder
  (`<data_dir>/spaces/<space>`, only that one) in `permissions.additionalDirectories`, with `Read` and `Edit` denied
  for the instances' login files in it. Its file tools can't edit:
  - other instances' folders (up to 50 other instances; beyond that, only the next items apply);
  - anything in any instance's config dir that Claude Code loads, runs or restores at a later launch, however many
    instances there are: settings, `.claude.json`, the cached `remote-settings.json` and `policy-limits.json`,
    instructions (`CLAUDE.md`, `rules/`, agents, commands, skills, output styles), plugins, hooks, shell snapshots,
    auto memory and transcripts (`projects/`), Codex's config dir;
  - another instance's auto memory, which its next run would load: each instance's Claude Code auto memory now lives
    outside the space folder, in `<data_dir>/automemory/<space-id>/<instance>` (set as `autoMemoryDirectory`). The
    memory an instance kept in its config dir is copied there at its first launch on this release; the old copy
    stays in place. A relocation retains or deletes it with the space folder. A profile's own
    `autoMemoryDirectory` is now ignored (the node logs a warning).

  These rules cover every space's instances on the node, also when a profile adds the node's whole `spaces/` folder
  as a directory.

  At every launch the node removes dangerous keys from the instance's `settings.json` unless the profile or SpaceMan
  set them: `disableAllHooks`, `hooks`, `env`, `*Helper`, `statusLine`, `autoMode`, `permissions`, plugin keys and
  similar. The removed key names are logged. Keep such keys in the profile; edits made to an instance's file by hand
  are removed. The hook fails closed: if the agent binary is missing or fails, its command still prints a deny.

  Both are merged with the profile's settings: its hooks (any `PermissionRequest`
  hook or `PreToolUse` guard), deny rules and directories are kept, and nothing is repeated. If your profiles carry a
  workaround (a dialog-denying hook script, or the whole `spaces/` folder as an additional directory), you can remove
  it. The node adds both, so they take nodes on this release or later.
- **Agents no longer ask in the terminal** (ADR-0066). Claude Code runs get `AskUserQuestion` and `EnterPlanMode` in
  the run's `permissions.deny`, so the model never sees the terminal question picker or plan mode (which ends in a
  plan-approval dialog): both left an unattended run waiting silently for an answer typed into its terminal. They are
  appended to the profile's own deny rules (duplicates dropped; its other permission keys kept). SpaceMan's orientation
  and the starter role prompts say to ask through `spaceman_human_ask` instead. A profile that lists one of the tools in
  its own `permissions.allow` keeps it.
  The node adds the deny, so it takes nodes on this release or later. Starter prompts already added to an installation
  are not rewritten; the orientation reaches every run.
- KB support sessions and dreams refuse a space role that no longer runs as kind kb or dreamer before launching,
  instead of launching a run and killing it.
- Claude Code runs start with its classic renderer (`"tui": "default"` in the run's settings) unless the profile's
  Claude Code settings set `tui`. Its full-screen renderer, which Claude Code otherwise often picks itself, keeps earlier output
  out of the terminal, so a watcher could not scroll back. Set `"tui": "fullscreen"` in a profile to keep it (ADR-0064).
  The node applies this default, so it takes nodes on this release or later; runs on older nodes keep Claude Code's
  own choice, and the profile's resolved settings in the API don't show it.
- Tests: most tests run in parallel; `make race` caps per-package parallelism with `RACE_PARALLEL` (default 4).

### Fixed
- The MCP servers page's inline guide link (also behind the server drawer) is underlined, not told apart by colour
  alone (axe `link-in-text-block`, WCAG 1.4.1). The SpaceMan MCP browser test now runs axe on the list and the drawer.
- **Opening the database waits out a brief lock instead of failing.** The SQLite busy timeout now applies from a
  connection's first statement. Before, the writer ran its first statement without one, so opening the data directory
  failed at once with `storage busy: open database: database is locked (SQLITE_BUSY)` whenever another connection held
  the file for a moment, for example the last connection of a store that had just closed, while it checkpointed the WAL.
  Closing the store now also waits until the connections still in use by unfinished requests have closed, and only
  then releases the data directory's lock. The checkpoint and that wait share one 8 s deadline, so a stop stays inside
  the units' `TimeoutStopSec=45s` (30 s `shutdown.timeout`, 5 s for gateway mints, 8 s for the database). Past the
  deadline the lock is released anyway, and central logs `closing central failed`; offline commands log it too. Seen as
  a flaky central restart in tests.
- Codex runs no longer leave their environment in the instance's `CODEX_HOME/shell_snapshots`: SpaceMan now turns off
  Codex's shell snapshot (`features.shell_snapshot=false`), and every Codex launch removes the `shell_snapshots` folder
  an earlier run left (without following links; the launch fails if it is still there). Found with codex-cli 0.166.0.
  **Rotate keys:** a Codex run on an earlier release kept its environment there, the run's SpaceMan token and, for a
  direct account with an API key, that account's `OPENAI_API_KEY`/`CODEX_API_KEY`. Run tokens are revoked when their
  run ends, but an account's API key is not: if a node ran Codex with an API-key account on an earlier release,
  rotate that key at the provider.
- With the `next_launch` policy, a person's response to a control that had stopped waited for the whole hold before
  control was started for it. A response held for control's agent instance now counts as work for control, so control
  is started on that instance (within the `control_autostart` limits) and the response is delivered to it. A response
  held for a worker's instance still waits for that worker.
- A relocation whose source was unavailable marked the source's runs lost without ending them: no `run_ended` event,
  no end count, and nothing that reacts to a run's end (recordings, KB sessions, control) learnt of it. They now
  end like runs a node reports ended.
- Run terminals scroll back without a takeover (operator request). The wheel, and a drag on a phone, scroll through
  earlier output in the live view without sending anything to the run. While you read, new output no longer pulls you
  to the bottom, and **Jump to live** returns.
- During a takeover, the wheel no longer types arrow keys into a full-screen program, which made Claude Code show
  "Scroll wheel is sending arrow keys". It sends mouse-wheel reports when the program asks for mouse input (ghostty-web
  0.4.0 turned every wheel step in an alternate screen into arrow keys).
- The run terminal no longer flickers between two font sizes on some window widths (the font fit now measures the
  canvas instead of predicting it).
- Tests: `internal/app` TestFleetLifecycle no longer fails at random with "timed out waiting for runs running after
  central restart". Reopening the store checks every reader connection, one per CPU by default, and under the race
  detector on a 32-CPU host that took 5-7 s, longer than the 5 s lease plus 1 s grace the test then had. The agent then
  stopped the runs, as it should without central (SM-NODE-007), so they never returned to running. The fleet test
  central now uses 4 readers (new test-only `app.Options.StoreReaders`), which brings a restart to about 1 s; restart
  tests also get a 30 s lease as a margin under load, and the test fails at once, with the cause, if a restart takes
  5 s or more. Product behaviour is unchanged.
- A person's response to a human action whose requester had ended (for example control's own question, answered after
  control stopped) became undeliverable when no control was running: control was not auto-started for it, and a
  control started later was not told about it. With control auto-start on, it now goes to control, starts control,
  and the control (or one launched by hand later) is told once about each unacknowledged response (ADR-0056 note).
- The "New inbox item" dialog focuses the item text when it opens, instead of the title.

## [0.1.0-rc.9] - 2026-11-07

Ninth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): Claude Code settings on
harness profiles (ADR-0059) with their review fixes; faster CI and `make check`, with the release workflow gated on
vet, race tests, coverage, fuzz smoke and the full site check (ADR-0062); a fix for the queue page crashing on
duplicate task titles; more built-in GitHub event templates and optional template filters (ADR-0061); live updates
that reconnect after central restarts; terminal and mobile UI fixes, with form descriptions under their fields;
relocation of spaces whose repositories use SpaceMan SSH keys; and the Harbor Pilot gateway contract with ADR-0060
(Proposed; documents and an opt-in test harness, no product behaviour). Upgrade central and the node agents. The
relocation fix needs both: an older agent keeps working, but relocating a space with keyed repositories to it still
fails. The relocation fix changes no node protocol, but Claude settings on profiles move it to 1.8: a run of a profile
with `claude_settings` on an older agent fails as `node_too_old`. One migration, 0039 (`claude_settings_json` on
harness profiles), which is forward-only and cannot be reversed: take a backup before upgrading (central also writes a
pre-migration snapshot; going back means restoring it). The new event templates are seeded at central's start, not by
a migration.

### Added
- More built-in GitHub event templates (ADR-0061), seeded at central's start on new and existing installations:
  - `github-pr-comment` and `github-issue-comment`: new comments on pull requests and on issues;
  - `github-pr-review`: submitted reviews (approved, changes requested, commented);
  - `github-pr-review-comment`: new inline comments on a pull request's diff;
  - `github-workflow-run` and `github-check-suite`: completed GitHub Actions runs and check suites, by default only
    failed, timed out, cancelled or startup failures (`include_successes` takes every conclusion).

  Each takes the account and optional `repository`, `owner` and `exclude_author` (for a space's own agent account).
  Comment and review templates ignore bots unless `include_bots` is set; CI templates can drop them with
  `exclude_bots`. Comments and reviews correlate with their pull request or issue, CI with its commit. Item titles
  hold no free text from the event: titles, comments and workflow names are only in the body and the payload. A
  built-in whose slug one of your templates already uses is not seeded; central logs a warning naming the slug.
- Template filters can be optional: `if_set` applies a filter only when a variable has a value, `unless_set` only
  when it has none. A new operator `absent` matches a missing or null field.
- Claude Code settings on harness profiles (ADR-0059, security-relevant): a claude-code profile's `claude_settings` are
  settings.json keys, any keys (permissions, auto mode rules, env, hooks, …). They are set in each run's own
  `settings.json` at launch, and keys taken out of the profile are removed at the next launch. The profile form edits
  permissions, auto mode rules (with a switch to keep or replace Claude Code's defaults) and environment variables,
  and other keys as JSON. `permissions.defaultMode` is held to the permission mode ceiling. Nodes need this release
  (protocol minor 8); a run of such a profile on an older node fails as `node_too_old` instead of starting without
  them. Migration 0039.
- Groundwork for routing run model traffic through a Harbor Pilot gateway (no product behaviour yet):
  - a contract pinned to Harbor Pilot v0.1.1 (`docs/contracts/harbor-pilot.md`);
  - ADR-0060, **Proposed** and security-relevant, with the operator's answers to its design questions recorded (one, routing of unscoped events, still open);
  - requirements SM-GW-001…010;
  - an opt-in test harness that runs a real `harbor-pilot serve --demo` (`make test-harbor-pilot`, `SPACEMAN_HARBOR_PILOT=1`); the gateway stops with the test binary even when its cleanup never runs, and a cached binary is reinstalled unless it is the pinned build;
  - a recorded spike: Claude Code 2.1.317 and interactive codex-cli 0.166.0 both reach the gateway with only a
    per-run token in their secret environment. Codex needs no login seeding.
- `SM_CHROMIUM_ARGS`: extra Chromium launch arguments (space-separated) for `make e2e`, `make screenshots` and
  `make site-check`, for hosts whose sandbox stops Chromium from starting (for example
  `--no-sandbox --disable-gpu --no-zygote` in a container). Unset by default and in CI.

### Changed
- Form field descriptions are shown under the field instead of between the label and the field, so fields in one row
  line up whether or not they have a description. The validation error comes after the description. One theme
  default (`inputWrapperOrder`) applies it to every form.
- On a phone the app bar shows only the logo, not the "SpaceMan" name, so it no longer stacks over a page header with
  the same name.
- Faster CI and `make check`:
  - test stores and test centrals start from a database that was migrated once per test binary, instead of applying
    every migration again for each test (about 10 s per store under the race detector);
  - CI runs lint and vet, race tests and the coverage floor as parallel jobs, each with its own Go cache;
  - `make fuzz-smoke` lists fuzz targets only in packages that declare one, and fails when listing them fails or
    finds none.
- CI scope (ADR-0062):
  - the coverage step allows 250 ms per write transaction on GitHub's hosted runners; locally it stays 100 ms;
  - on pull requests the site's accessibility audit runs only when site, docs, web, API or changelog files change;
    the site build, links, Mermaid and reference drift still run every time (`make site-verify`);
  - fuzzing runs nightly and in the release workflow, not on pull requests.
- The release workflow builds nothing until vet, race tests, the coverage floor, fuzz smoke and the full site check
  with its accessibility audit pass.

### Fixed
- Relocating a space whose repositories use SpaceMan SSH keys no longer fails in Prepare with "Host key verification
  failed" on a destination without Git credentials of its own. The destination clones such a repository from its
  configured URL with its key and pinned host keys, as a normal clone does. Central matches each relocated repository
  to the configured one (by folder, else by remote URL) and puts the key only on the message to the destination
  agent: stored commands, logs, the journal and the audit never hold it. Repositories without a key clone with the
  node's own Git authentication as before. An older destination agent ignores the key (no protocol change). A
  relocation also no longer drops the user name of an `ssh://git@host/…` remote URL.
- Terminals: the agent tab strip no longer scrolls vertically or shows scrollbars, and a tab's first line (status
  dot and name) no longer scrolls out of view on a phone. Tabs keep their content's size; when there are more than fit,
  the strip scrolls sideways (touch, trackpad or mouse wheel), snapping to tabs, and keeps the selected tab in view.
- Run header: the turn number and the state no longer get clipped ("#." / "working ·…"); the role name truncates
  first and the actions wrap to a second row when there is no room. On a phone the header is two rows: who (status,
  role, turn, state) and the actions (Live/Replay, then Take over, Stop and the details toggle as icon buttons).
- Terminals on a phone: the page no longer scrolls sideways (a terminal wider than the screen scrolls inside its own
  box), the run inspector goes under the terminal instead of beside it, the replay controls wrap, and the Terminals
  workspace's space and agent list is a drawer opened from the space name (a long name ends in an ellipsis).
- Live updates no longer stay "offline" after central restarts. A change feed that the browser gave up on (a 502
  while central restarts, a 401) or that the server ended is reopened with backoff (1 s doubling to 30 s, with jitter,
  reset once it opens): each try re-checks the session first, sending the browser to sign-in when the session is gone,
  and resumes from the last change seen. A stream the server ends as unauthenticated while the session is still valid
  reconnects at once the first time; when fresh streams keep ending that way, each further reconnect waits for the
  next backoff step. The header shows "offline" while waiting and "connecting" while trying. The
  tab holding the stream retries at once when the window comes back online, when it becomes visible again, and when
  another tab opens or reloads, so a stale background tab can't keep every tab offline. The tabs' lock and channel are
  now `spaceman-watch.v2`, so tabs still running an older build don't hold the new ones offline.
- The queue page no longer crashes ("Duplicate options are not supported") when two tasks share a title. Task pickers
  (a task's dependencies, a new task's parent and dependencies) use task IDs as values; a title shared by several
  tasks shows each task's status and the end of its ID. The run launcher's "another role" choice no longer collides
  with a space role named `custom`. The queue route has its own error boundary, and its task drawer and dialogs are
  contained separately, so a failing part shows an alert with a retry instead of blanking the page.
- Saving a harness profile without `claude_settings` keeps the stored settings, read in the save's own transaction; a
  failed read before the save could clear them.
- Changes to a profile's `claude_settings` are audited on `profile.created` and `profile.updated` with the changed
  top-level keys and a sha256 of the new settings, never the values.
- The node records the settings.json keys it owns before writing settings.json. When that record is corrupt, it moves
  `settings.json` and the record aside (`.corrupt-<unix nanoseconds>`), logs a warning naming both, and starts from fresh
  settings: no key an earlier profile set survives, and the instance recovers without a fix on the node.
- A run that can't be dispatched (`node_too_old`, `account_unavailable`) is now failed and its launch cancelled; ending
  it used to fail on a database constraint (a NULL session ID), which left the run pending. Its end is handled like a
  reported one: counted in `spaceman_runs_ended_total` (reason `launch_failed`), published once as run_ended, and a
  draining relocation of its space goes on. Failing an already-ended run only cancels its commands.
- Tests: `internal/events` TestReplayIsExplicitAndDistinctFromRetry, TestDurableIngestMetrics and other ingestion tests
  no longer read deliveries or metrics once right after waiting for the inbox item. The item is committed before its
  delivery is recorded and the metric after that, so the read could come too early (seen in CI as an index out of
  range panic and "durable created = 0"). Failure messages no longer index possibly empty lists.
- Tests: `internal/keyring` TestLoadKEKSources and `internal/cli` TestTokenSources pass under a restrictive umask (such
  as 077). They now chmod their deliberately world-readable files instead of relying on the mode given to WriteFile,
  which the umask narrows. Product code is unchanged.

### Security
- Profile settings may set endpoint and credential `env` variables, helpers and hooks: recorded as an accepted risk
  (ADR-0059, THREAT_MODEL T-32).

## [0.1.0-rc.8] - 2026-11-06

Eighth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): shared Git SSH keys, so one
key on a Git account clones many private repositories, and a clearer host-key error for repositories without a key.
Shared keys work with node agents from 0.1.0-rc.6 on (node protocol 1.7). The clearer error needs this release's agent.

### Added
- Shared Git SSH keys (ADR-0058, security-relevant). An administrator generates one key, adds its public half to a Git
  account (preferably a read-only machine user), and picks the spaces that may use it. Those spaces' repositories
  choose it instead of their own deploy key.
  - API: `/api/v1/git-ssh-keys`, and `/spaces/{id}/git-ssh-keys`.
  - CLI: `spaceman git-key`, and `spaceman repo update --git-key`.
  - UI: a Git SSH keys page, and a Shared Git SSH key choice on each repository.
  - Migration 0038.

### Fixed
- A host-key failure on a repository without a SpaceMan SSH key no longer talks about pinned known_hosts. Those pins
  apply only to keyed repositories. The message now says the node's own SSH setup does not know the host, and how to
  fix it: an `https://` URL for a public repository, a SpaceMan SSH key, or the node user's known_hosts.

## [0.1.0-rc.7] - 2026-11-06

Seventh release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): the first pass of the UX
review (Terminals views, one space layout, dark-theme status colours) and a security update of klauspost/compress. Only
central changes; node agents from 0.1.0-rc.6 keep working.

### Security
- `github.com/klauspost/compress` 1.20.2 fixes GO-2026-6865, which govulncheck reports as reachable through the zstd
  encoder used for terminal recordings.

### Added
- Watching agents (UX review §4):
  - A space's **Terminals** tab shows every running agent as a tab: a status dot, `role #turn` and its state in words.
    The selected agent's terminal is below. Runs that ended in the last day are under **Ended**.
  - The **Terminals** workspace (sidebar) lists every space with its agents next to the selected space's tabs, with a
    status bar of running agents and those waiting for input. Space and agent are in the URL.
  - `[` and `]` switch between agents.

### Changed
- Every space page, its run pages, inbox items and subscriptions share one layout. The space's header (name, state, node,
  Launch run, Pause, and a ⋯ menu for edit, export, relocate and the lifecycle) and its section tabs stay put, and a
  section has a one-line title. The run page is in the layout, with a "Terminals / control #4" breadcrumb, so you can go
  from a terminal to any section of its space.
- A run's page has a one-line header (status dot, role, kind and harness, turn, state, Live/Replay, Take over, Stop) and
  an inspector beside the terminal:
  - Now: state, turn, duration, human actions it waits on;
  - Task;
  - Usage: tokens and estimate;
  - Placement: node, account, worktree;
  - Launch and configuration, and Identifiers, both collapsed.
  The nine identifier cards are gone, an ended run shows a one-line summary, and the takeover toast is gone.
- The space overview leads with its running agents (with links to their terminals) and puts the run history at the
  bottom. The lease card is gone, and an invalid lease shows as an alert.
- In the dark theme, dimmed text is slightly lighter, so it stays readable on hovered and selected rows.
- UX review (docs/UX_REVIEW.md), first pass:
  - In the dark theme, status badges and alerts get their colour back: Ready, Waiting, Pending and Open had all looked
    the same grey, and so had warnings such as "not encrypted".
  - Badges are in sentence case, and dialog and drawer titles are bold.
  - The type scale is one step larger: help text was 11 px.
  - Row actions are 28 px targets, and monospace inputs keep normal labels.
  - Queue rows show one state ("Waiting" or "Blocked · n" instead of READY plus WAITING), and running tasks link to the
    run working on them. "Next" is one line, with the reasons a task waits in a tooltip.
  - Messages shows people and agents by default, with SpaceMan's own notifications under a System filter.
  - Attention shows "Never backed up" instead of OK when no backup has run.

## [0.1.0-rc.6] - 2026-11-05

Sixth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): private repositories cloned
with SSH keys SpaceMan holds. Central and the node agent both need this release (node protocol 1.7).

### Added
- Repository SSH keys held by SpaceMan (ADR-0057, security-relevant). A repository with an SSH URL can have an Ed25519
  key that central generates and keeps sealed; only the public key is shown, to add as a read-only deploy key. The node
  clones and fetches with it over SSH, against pinned host keys (GitHub built in, `known_hosts` per repository for
  others), and agents never receive it.
  - API: `POST`/`DELETE /spaces/{id}/repositories/{id}/ssh-key`.
  - CLI: `spaceman repo ssh-key` and `spaceman repo ssh-key-remove`.
  - UI: the repository's SSH key card.
  - The node protocol goes to 1.7.

### Fixed
- Repository clone and fetch errors caused by SSH host keys are now one line that says what to fix, instead of ssh's
  warning banner.

## [0.1.0-rc.5] - 2026-11-04

Fifth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): control starts for waiting
work and stops when idle, nodes can be edited (and provisioned with Terraform), and the step-up prompt asks for the
authenticator code.

### Added
- Control starts and stops on its own (ADR-0056). When work waits for control (an inbox item, a task to decide on, a
  worker that ended, a message) and no control run is live, SpaceMan launches the space's control role, resuming the
  last control's session. It skips this while the space is paused or its node is away. When the launch is refused it
  raises a human action, and it stops after six starts an hour. A control idle for 30 minutes is stopped, and the next
  work starts it again. These are two new space limits, `control_autostart` (on by default) and
  `control_idle_stop_minutes` (30; 0: never), on the Limits page as **Start control on new work** and **Stop idle
  control**.
- `PUT /api/v1/nodes/{id}` and `spaceman node update` edit a node's name, description and labels (with `If-Match`). The
  Terraform provider's new `spaceman_node` resource uses them to register nodes and hand out enrollment tokens.

### Fixed
- The "Confirm it's you" step-up prompt opens above the dialog that triggered it, not underneath it. For an account with
  an authenticator it asks for the code together with the password. Before, it asked for the password only and then
  showed "multi-factor authentication required".
- A restarted control resumes its session only when the session finished a turn. Resuming a session that was never
  saved fails, so such a restart now starts a fresh session on the same instance.

## [0.1.0-rc.4] - 2026-11-04

Fourth release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md), with a fix found on the
first deployment.

### Fixed
- A new run is told about messages an earlier run of the same addressee saw but never acknowledged. Before, a control
  launched after one that ended without handling its notice (for example because its account was refused) was never
  told about the waiting inbox item.

## [0.1.0-rc.3] - 2026-11-04

Third release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md): the first with images on
Docker Hub.

### Added
- Release images on Docker Hub (ADR-0055): `docker.io/aurimasniekis/spaceman` and `docker.io/aurimasniekis/spaceman-agent`
  for linux/amd64 and linux/arm64, pushed by the release workflow for every `v*` tag, with SBOMs and provenance
  attestations.

## [0.1.0-rc.2] - 2026-11-04

Second release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md). Replaces 0.1.0-rc.1, whose
unpublished drafts were withdrawn: Codex runs with an OpenAI API key did not start there.

### Fixed
- Codex runs with an OpenAI API key start working instead of stopping at Codex's sign-in screen: interactive Codex
  0.166.0 ignores `OPENAI_API_KEY`, so the node now also stores the key as an API-key login in the instance's
  `CODEX_HOME` (ADR-0054). Found by a dry run of the new real Codex suite.

### Added
- `make test-real-codex` (a real Codex turn through SpaceMan) and `make test-real-s3-external` (the artifact store and
  backups against your own S3 or R2 bucket), with a checklist for verifying a release on another server
  (docs/TEST_STRATEGY.md).

## [0.1.0-rc.1] - 2026-11-04

First release candidate (pre-release; not production-ready, see docs/RELEASE_READINESS.md).

### Fixed
- Terminal notices reach agents after their first turn. herdr-core's idle gate refuses a program that reports `done`,
  which Claude Code does after every turn, so new inbox items and messages were never announced; a refused notice now
  also retries at the next idle report instead of 5 minutes later (ADR-0052).
- Leaving a run page and coming back no longer hangs the browser tab when the run is a full-screen program such as
  Claude Code (a ghostty-web 0.4.0 problem with disposed terminals, avoided by resetting the terminal first).
- Control is told about tasks someone else adds to the queue (like new inbox items) and about tasks someone else makes
  ready, for example from the backlog, so an operator's task no longer waits until control happens to look at the
  queue (SM-CTRL-002).
- The run terminal follows the cursor and scrolls with the mouse wheel, so the bottom of a tall terminal is reachable.
- Several SpaceMan tabs no longer exhaust the browser's connections over plain HTTP: tabs share one change-feed stream,
  and hidden tabs release their live terminal.

### Changed
- **Security:** a diagnostic run's launch needs `run:diagnose` on top of `run:control` (SM-CTRL-004, ADR-0050).
  Operators now hold `run:diagnose`, so operators and admins keep launching diagnostics; viewers never could.
- **Security:** an incident task from a source the queue policy does not trust no longer jumps the queue or preempts
  work; by default it becomes a proposal (ADR-0050).
- Purging a space deletes its runs' recordings and transcripts from the artifact store after the purge commits; they
  were left in the data directory before (ADR-0048).
- The Terraform provider moved to its own repository, `terraform-provider-spaceman` (module
  `github.com/aurimasniekis/terraform-provider-spaceman`), with its history (ADR-0047). This repository no longer contains
  it, and `make check`, CI and the Makefile no longer refer to it: its unit and acceptance tests run there, and the
  acceptance tests build central from a SpaceMan checkout (`SPACEMAN_SRC`, or `../spaceman`).

### Added
- Codex ChatGPT logins (SM-ACCT-002, ADR-0053, migration 0036): the account kind `codex_chatgpt_login` takes the
  `auth.json` that `codex login` writes. The node keeps one live copy per account, which every Codex instance there
  links to, so Codex's own refreshes survive later launches; a rotation replaces it. Use one login on one node.
- Codex runs get their skills in `$CODEX_HOME/skills` (SM-MCP-004; loading verified with codex-cli 0.166.0).
- Starter roles (SM-PROMPT-005, ADR-0051): a new installation gets example harness profiles, prompts and role templates
  for control, planner, implementor, reviewer, kb and dreamer, added once and editable or deletable. A space gets
  ready-to-launch roles from them with **Add starter roles**, `spaceman role add-starters`, `POST
  /spaces/{id}/starter-roles`, or `starter_roles` when it is created (ticked by default in the UI).
- `scripts/dev-stack.sh restart DIR`: rebuild and restart central on a dev stack's existing data.
- Agent setting `runner.terminal_size` (default `160x48`): the size programs start at, instead of herdr-core's 200×120.
- Role templates and harness profiles can be deleted (`DELETE /role-templates/{id}`, `DELETE /harness-profiles/{id}`,
  `spaceman role template delete`, `spaceman profile delete`, the Roles page) while nothing uses them.
- Incident policy (ADR-0050, SM-QUEUE-009, migration 0034): a space's queue policy names the **trusted triggers** that
  may raise incidents (`operator`, `control`, `subscription:<id>`, `template:<id or slug>`; default operator and
  control); an incident from any other source — including one control relays from a worker's, schedule's or another
  space's inbox item — is **proposed** (an ordinary task an operator confirms or dismisses, `POST
  /spaces/{id}/tasks/{task}/incident`, `spaceman task incident`) or **refused**, as the policy says. Preempted tasks'
  workers **finish** (told to wrap up), are **paused** (their turn interrupted with the existing `run.interrupt`
  command) or are **interrupted** (stopped, session resumable), recorded per run in a preemption log and audited;
  **incident capacity** adds top-level slots for incidents; **resumption** is manual (control is told) or automatic
  when the incident ends, and `POST .../resume`, `spaceman task resume` and the control tool `spaceman_task_resume`
  resume one task. Control gets a notification for each step. Queue policy dialog, task drawer (confirm, dismiss,
  resume, preemption log) and Terraform `spaceman_space_queue_policy` fields.
- Human-action type permissions (ADR-0050, SM-HUI-008): per space and type, who sees, asks with and responds to it
  (weakest role) and which agent roles may ask; enforced in the lists, counts, reads, responses and creation; managed
  by space admins (`human_type_permission:write`) under `/spaces/{id}/human-type-permissions`, in the space's human
  actions page, `spaceman human permission` and Terraform `spaceman_human_action_type_permission`.
- User profile edits (ADR-0050, SM-API-004): `PUT /users/{id}/profile` (global admin, step-up, If-Match, audited)
  changes a user's email, display name and administrator MFA setting; the users API returns `mfa_required_set` beside
  the effective `mfa_required`. **Edit** on the Users page, `spaceman user update`, and Terraform `spaceman_user` now
  updates these fields in place instead of refusing them.
- S3 artifact store (ADR-0048, SM-STORE-002): `artifacts.backend: s3` keeps attachment content, central's copies of
  recordings and run transcripts in an S3-compatible bucket (`artifacts.s3`, keys from files, TLS always verified) under
  the same keys as the local layout. Uploads are spooled and sent as single PUTs whose SHA-256 the service checks;
  downloads stream, answer range requests with range GETs, and fail at the end when attachment or transcript content is
  damaged. Recordings are spooled while a node ships them and archived when they close or their run ends; transcripts
  are uploaded once received and checked; an upload the bucket refuses stays on the local disk, is served from there and
  is retried at the next start. Retention, purge and garbage collection delete the objects; quotas are unchanged.
  Backups copy every S3 artifact into the sealed backup and record the store in the manifest (`artifact_store`). The new
  offline command `spaceman artifacts migrate --to s3|local [--remove-source] [--dry-run]` moves stored artifacts, and
  `spaceman backup restore` names it when the installation uses S3. Configuration validation refuses an
  `artifacts.s3.prefix` that overlaps `backups.s3.prefix` in the same bucket. Operators' page: Artifact storage.
- Go interfaces for the remaining adapter kinds (SM-ARCH-003, ADR-0048): `artifacts.Store` (local and S3),
  `events.Source` (Pulse durable consumers and core NATS, picked by delivery mode) and `gateway.Transport` (streamable
  HTTP). The implementations are unchanged; tests drive each through a fake.
- Paused-space event policies (ADR-0049, SM-EVT-014): each subscription's `while_paused` is `defer` (default: keep
  ingesting, tell control about new items when the space resumes), `wake` (tell control at once) or `stop` (stop until
  the resume; durable events wait in the stream). Deferred notices go out on resume and at central's start, without
  polling. Subscribe dialog and subscription page, `while_paused` in the API, `--while-paused` in the CLI; inbox items
  show **control told on resume**; subscriptions report the state `paused` (also in `spaceman_ingest_subscriptions`).
- Bounded automation across installations (ADR-0049, SM-EVT-016): outbound events carry the SpaceMan header
  `spaceman-depth` (how many SpaceMan ingestions led to the change); inbox items and tasks created from them record
  their `automation_depth`; an inbound event at or over the space's new limit `event_depth` (default 8, 1–64), or with
  an unreadable depth, is quarantined with the reason. Reprocessing it ingests it one level deeper.
- Routed human responses as inbox items (ADR-0049, SM-INBOX-001): the human-action policy field `response_inbox`
  (`off` by default, `routed`, `always`) makes answers also inbox items with the source `human_response`. The item
  names the action and its routing, never the response, which stays on the action. Policy dialog, API,
  `--response-inbox` in the CLI.
- Migration 0033 (`while_paused`, `notice_deferred`, depth columns, `response_inbox`).
- Space repositories (ADR-0044, SM-REPO-001/003, SM-SPACE-001, SM-UI-002, SM-API-004): a central list per space (name,
  folder, clone URL without credentials, default branch, role code or kb, clone policy auto, on_demand or manual) with
  API CRUD under `/spaces/{id}/repositories` (If-Match, audited), a sync endpoint, the node protocol command
  `space.repos.sync` (protocol minor 4) that clones, fetches and reports with the node's own Git authentication, a space
  **Repositories** page with the node's report (remote, default branch, last fetch, changes, unpushed commits, worktrees
  and their runs), worktree links on runs and tasks, the read-only agent tool `spaceman_repos_list`, `spaceman repo`
  commands and the Terraform resource `spaceman_space_repository`. Workers' worktrees and first launches clone configured
  repositories; relocation clones configured repositories on the destination. Migration 0029 lists every knowledge
  base's folder as a repository (role kb, manual); knowledge bases are unchanged.
- Retention (ADR-0045, SM-REC-003, SM-HUI-007, SM-ATT-006, SM-SPACE-001): retention periods for recordings, transcripts,
  attachments of finished items and finished human actions are limits (`limits.retention_*_days`, overridable per space
  on the Limits page, API, CLI and Terraform `spaceman_space_limits`; 0 keeps, the default), and audit retention is
  installation-wide (`retention.audit_days`). One daily pass on the shared deadline timer deletes what is older, never
  what an open item uses and never before a restore is acknowledged; every deletion is audited and in the change feed.
  Status and a manual pass: Backups page **Retention** card, `GET /retention`, `POST /retention/run`,
  `spaceman system retention status|run`. Changing a retention period needs the new `retention:write` (admins).
- Space export (ADR-0045): `GET /spaces/{id}/export` streams a ZIP of the space's inbox items, tasks, human actions with
  responses and comments, messages and attachments (JSON Lines plus files, with a checksummed manifest), from one
  consistent snapshot; `GET /spaces/{id}/export/summary`, `spaceman space export summary|download`, an **Export** dialog
  on the space overview. Admins only (`space:export`), audited, bounded by the space's `export_max_bytes` (default
  2 GiB). The archive is **not encrypted**, and says so everywhere it is offered.
- Configurable recording size: the space limit `recording_quota_bytes` (default 512 MiB, 1 MiB–64 GiB) is sent to the
  node with each launch (SM-REC-003).
- Durable forward audit cursor (SM-OBS-003): `GET /audit?after=<seq>` lists events oldest first with `next_after` to
  resume from, and a `gap` when retention pruned events past the position; `spaceman audit list --after`. The audit page
  shows when older events were pruned.
- Backups flush role memory first: central asks connected nodes to checkpoint and sync (node protocol minor 5,
  `memory.flush`), waits at most a minute, and the manifest, the Backups page and the `backup.finished` audit event name
  spaces whose memory is possibly stale (disconnected or older nodes, timeouts, unacknowledged history) (SM-MEM-005).
- SSO-only sign-in (password sign-in off for the installation), with a lockout precondition and guards, as in Hallpass:
  Sign-in providers page, `PATCH /api/v1/oidc/settings`, `spaceman oidc settings set --sso-only` (SM-AUTH-001).
- Direct sign-in through one provider (`/login?password=1` shows every method).
- RP-initiated logout per provider (`end_session_enabled`, API, UI, CLI `--end-session`, Terraform
  `spaceman_oidc_provider.end_session_enabled`): signing out of an SSO session offers sign-out at the provider.
- Attention summary (SM-UI-003): an **Attention** page, `GET /api/v1/attention` and `spaceman system attention` gather
  what needs a person: pending, overdue and escalated human actions, blocked and failed tasks, quarantined events,
  unknown and recently failed runs, budget notices, attachment storage near its quota, spaces whose node is not
  connected, and, with `system:read`, disconnected nodes, outbound dead letters, failing backups and central's storage
  health (readiness checks, database state, data-disk free space against its reserve). Each category is filtered by the
  caller's permissions; one grouped query per category, refreshed from the change feed. Each space overview shows its
  counters.
- Terminal takeover (SM-UI-004, ADR-0046, **security-relevant**): an admin holding `terminal:takeover` can take over a
  running terminal from the run page (and `spaceman run takeover`). Keystrokes go through herdr-core's interactive
  attach (contract S-5), are bounded and never stored or logged; every automated input (Message the agent, message and
  human-response notices, queued node input) waits until the takeover ends; every viewer sees who holds it; it is
  audited and ends on release, expiry (30 s without renewal), run end or node failure. Node protocol 1.6.
- Human actions (SM-HUM-006, SM-HUI-001, SM-HUI-006): server-side search (`q`) over title and text, filters for status,
  assignee (`me`, `unassigned`), priority, requester, due date and space, and display sorts, on both lists and the
  CLI; **Ask the requester** clarification requests (`POST …/clarify`, a durable `clarification` notice routed like a
  response) and the agent tool `spaceman_human_reply`; links from task blockers and actions to the requester run, task,
  inbox item and follow-up; per-user notification preferences (`/auth/me/notification-preferences`, Account →
  Notifications, `spaceman notifications`) for in-app and desktop notifications.
- Attachments beyond the inbox (SM-ATT-001/002): tasks, human-action requests, responses and comments, and messages and
  replies reference attachments (`attachment_ids`), written in the TipTap composer with paste, drop and attach.
- Space archive, restore and purge in the web UI (SM-SPACE-002), with confirmation dialogs; purge asks for the slug and
  a recent sign-in with MFA. Spaces lists archived spaces on request.
- Task reassignment in the task drawer and `spaceman task update --run` (SM-CTRL-004); the audit record of a task
  update names the previous and new assignee, and a run of another space is refused.
- Custom-definition subscriptions in the subscribe dialog (SM-EVT-017): a JSON editor with validation as you type and a
  sample-event preview through the preview API; a custom subscription shows its definition.
- TanStack Table (sortable, filterable shared table on the attention and nodes pages) and TanStack Store (per-browser
  view preferences) are now used (SM-UI-001); every form uses TanStack Form, including the schema-driven human-action
  fields (SM-ATT-007).
- Migration 0031: clarification notices and comment kinds, notification preferences, terminal takeovers.
- Release readiness report (`docs/RELEASE_READINESS.md`, site: Development → Release readiness): what is implemented,
  tested and verified for real, what is not, and what a production release still needs. It makes no production-readiness
  claim.
- Documentation site completed (SM-DOCS-001…005): new **Architecture**, **Deployment overview** (topology, install paths
  and a recovery map), **Troubleshooting** (startup errors, readiness codes, node connection, leases, recordings,
  sign-in) and **Publishing the site** pages; a **Configuration reference** generated from `internal/config`'s types,
  doc comments and defaults (`make config-docs`, drift test `TestConfigReferenceUpToDate` in `make check`, also checked
  by `make site-check`); a glossary of coordination terms on Concepts; space archive, restore and purge in the first-space
  guide; a `kb-sessions` screenshot of a knowledge base's support sessions. `check-mermaid.mjs` now also fails on
  unquoted flowchart labels, and `check-shots.mjs` on an inventoried screenshot that no page shows. The site is **not
  published**: the Pages workflow has never run.
- Complete remote CLI coverage (SM-API-003, ADR-0040 amendment): commands for 239 of the 248 API operations (was 132).
  New groups `kb`, `memory`, `message`, `dream`, `approval`, `attachment` (upload, download), `oidc` (providers, group
  mappings), `identity`, `session`, `mfa`, `password` and `watch` (the change feed, JSON Lines with `-o json`); new
  commands for task blockers, dependencies, escalation, decisions and the queue policy, run input, usage, recordings and
  transcripts (download to a file, delete with `--yes`), human-action creation, assignment, comments, types and policies,
  a space's accounts and MCP servers, prompt diff, fork, rollback and draft discard, skill creation from a directory,
  event template preview, subscription quarantine (reprocess, discard), user reset tokens, link invitations and
  identities, and the outbox, restore, break-glass and root-token notices of `system`. One-time secrets of the new
  commands are shown only on request (`--secret-file` or `--show-secret`); downloads go only to `--out` (new `0600`
  file, `--force` to replace, never a terminal). The nine operations left out are browser sign-in flows, listed with
  their reasons in the CLI guide and reference; `TestEveryOperationHasACommand` fails when a new operation has neither a
  command nor a recorded exclusion.
- Core NATS event subscriptions (SM-EVT-011, ADR-0042): `delivery: core` on a subscription (API, subscribe dialog, CLI
  `--delivery`, Terraform `delivery`) subscribes with plain NATS on the Pulse connection, for sources that publish
  without JetStream. Same filters, schema, quarantine and inbox items; deduplicated by `Nats-Msg-Id` or, without one, a
  content hash. Clearly labelled **non-durable** (API schema, a badge and explanation in the UI, the events guide): no
  replay, no redelivery, no cursor, and messages are lost while disconnected. Replay and start-now are refused; a missing
  `subscribe:<subject>` grant is reported on the subscription. Migration 0028.
- Ingestion metrics on the management listener's `/metrics` (SM-EVT-013, SM-OBS-002): `spaceman_ingest_deliveries_total`
  (delivery mode, origin, result: created, duplicate, filtered, quarantined, error, rate_limited),
  `spaceman_ingest_processing_duration_seconds`, `spaceman_ingest_consumer_pending` and `_ack_pending` (JetStream
  consumer lag), `spaceman_ingest_core_dropped_total`, `spaceman_ingest_subscriptions` and `spaceman_ingest_connections`
  by state, `spaceman_ingest_quarantine_open` and `spaceman_ingest_replays_total`. Labels are fixed enums only. New
  operators' page **Observability** with the full metric catalog and suggested alerts.
- Scripted end-to-end demonstration (SM-VER-003): `make demo` (`scripts/demo.sh`, driver `test/demo` behind the
  `demo` build tag, browser steps `web/e2e/demo.spec.ts`) runs the thirty steps of the demonstration in order against a
  real central with the embedded UI, two real agents with their own herdr-core cores, devpulse and devmcp, through the
  browser, the API and the CLI, and writes one evidence line per step (outside the repository) plus a summary table. Stub
  harness by default; `SPACEMAN_DEMO_REAL=1` with `SPACEMAN_DEMO_CLAUDE_TOKEN` makes the Claude Code worker a real,
  budget-bounded run. Guide: `docs/DEMO.md`, site page Development → End-to-end demonstration.
- Stub harness (development only): it now finds the SpaceMan MCP server when it stands in for Codex (`-c mcp_servers.…`
  overrides), and understands `/await-human [n]`, `/kb-delay <seconds>` and `/dream`; KB answers cite the working
  tree's revision.
- Real-dependency suites (SM-VER-002), opt-in behind the `real` build tag and a variable each, each removing only what
  it created: `make test-real-porch` (SpaceMan signs in through a real go-porch built from the sibling checkout:
  `porch.groups` mapping, group JIT, replay refusal, step-up), `make test-real-pulse` (real pulsed, nats-server and the
  go-pulse-github service with its own fixtures: fan-out to two spaces, dedup, replay, quarantine, loop guard, outbound
  publication), `make test-real-s3` (a throwaway MinIO container pinned by digest: backups, list, verify, tamper
  detection, offline restore), `make test-real-systemd` (`scripts/systemd-smoke.sh`: the linux release archive under
  systemd in a Debian 13 container, both units), and `make e2e-browsers` (the browser journey in Firefox and WebKit).
  Results from 2026-11-03 are in `docs/TRACEABILITY.md` and `docs/TEST_STRATEGY.md`. Codex, AWS S3 and a physical Linux
  host remain unverified (`docs/LIMITATIONS.md`).
- Remote command line (SM-API-003, ADR-0040): `spaceman` is now also a client for a running central's API
  (`--server`/`SPACEMAN_SERVER`, an API token from `--token-file`, `SPACEMAN_TOKEN_FILE` or `SPACEMAN_TOKEN`,
  `-o table|json|yaml`). Commands for nodes, spaces (including `space relocate precheck|start|status|retry|cancel|release`
  and the unavailable-source flow with an explicit acknowledgement), runs, inbox, tasks, human actions, schedules,
  limits, accounts, prompts, skills, harness profiles, roles and role templates, event connections, templates and
  subscriptions (with replays), MCP servers and grants, approval policies, cross-space peers, requests and settings,
  users, API tokens, audit and backups (`system backup`). Updates send `If-Match` with the version read (or
  `--if-match`), creates send an `Idempotency-Key`, `--all` follows cursors, problems are printed with their code,
  fields and request ID, and exit codes are documented (0 ok, 1 usage, 2 API error, 3 conflict, 4 not found, 5 auth).
  Generated reference in `site/reference/cli/` (`make cli-docs`, drift-checked), guide in `site/operators/cli.md`.
- API: read operations for objects that only had a list: `GET /event-connections/{id}`, `/harness-profiles/{id}`,
  `/role-templates/{id}` (ID or slug), `/spaces/{id}/roles/{name}`, `/approval-policies/{id}` and
  `/spaces/{id}/xspace/peers/{peer_id}`. The API reference now also describes the attachment-content and transcript
  downloads.
- Space relocation (SM-INV-003, SM-SPACE-005…008, ADR-0037): an operator moves a stopped space to another node from the
  space page (**Relocate**), the API (`/spaces/{id}/relocation`, with precheck, retry, cancel and release) or Terraform.
  The source syncs role memory and uploads, and its repositories are checked; unpushed, changed, untracked and stashed
  work is refused unless explicitly exported through central. The destination is prepared and verified, then the
  assignment and the execution lease move in one step with a new generation that fences the source; the space stays
  paused and the source folder is retained (or deleted). An unavailable source can be replaced from central's copies with
  an audited acknowledgement of possible loss. Runs on the destination start fresh native sessions (project continuity
  only). Node protocol minor 3; migration 0026.
- Terraform: a change of `spaceman_space.node_id` now relocates the space in place (`relocation_dirty_state`,
  `relocation_stop_runs`, `relocation_recovery`, `relocation_retain_days`, `relocation_timeout`) instead of being refused.
- Terraform provider phase 2 (SM-API-004, SM-HUI-008, ADR-0041): `spaceman_space_limits` (owns only the configured
  overrides; removing one clears it, destroy clears the owned ones), `spaceman_space_queue_policy` (top-level task
  limit), `spaceman_schedule`, `spaceman_space_xspace_settings`, `spaceman_xspace_peer` (one side of a cross-space grant,
  owned whole), `spaceman_human_action_policy` (installation defaults and space overrides), `spaceman_user` (disabled on
  destroy), `spaceman_user_grants` (the whole manual grant set) and `spaceman_user_identity`; data sources
  `spaceman_space_limits` (effective values and sources) and `spaceman_user`. Creating a peer configuration or a
  human-action policy that already exists fails instead of overwriting it. Passwords, reset tokens, link invitations and
  MFA enrollment are never managed.

### Security
- Retention, space export and SSO-only sign-in are security-relevant changes (ADR-0045): shortening a retention period
  (admins, `retention:write`) deletes data at the next pass; exports are plaintext and admin-only; SSO-only is guarded
  against an administrator lockout.
- Security review (M4-08, `docs/SECURITY_REVIEW.md`, ADR-0039). The fixes:
  - **Login and MFA throttles hold under parallel attempts.** Attempts on one account or second factor are serialized,
    so a burst can no longer try many TOTP codes in one throttle window.
  - **The node daemon no longer runs code from repositories agents can write.** Writer worktrees are prepared with
    hooks and fsmonitor disabled. Git refuses to run in a repository whose own configuration defines a program, such as
    a filter, merge or diff driver, SSH or proxy command, credential helper, include or URL rewrite. This covers memory
    checkouts, KB and writer worktrees, and relocation.
  - **Relocation keeps everything inside the destination's space folder.** Worktree paths are validated centrally and
    on the destination and may not pass through a symlink. Chained symlinks in the loose-file archive and a symlink in
    place of `instances/` are refused.
  - **Purging a space now forgets its grants immediately.** The authorization engine used to keep them in memory until
    its next reload.
  - **Concurrent requests with one `Idempotency-Key` create the resource once.**
  - **Agent tools:** only a recipient may reply to a message, and so resolve it.
  - **API:** deleting a transcript needs re-authentication with MFA, like deleting a recording. Recording a queue
    decision (`GET …/queue/next?record=true`) needs `queue:write`.
  - **Event templates:** instructions may only use the template's variables (`vars.*` placeholders) and never contain event text.
  - **Documentation:** the threat model and the Security page now state every SM-SEC-001 boundary, and
    `make traceability` checks that they keep doing so.
- Cross-space requests (SM-XSPACE-001…003, ADR-0038): a space's control can ask another space a question, delegate
  work to it or ask one of its knowledge bases, only as far as both spaces allow. Each space sets, per peer, what it
  accepts (discovery, questions, delegation, knowledge per KB, what its replies disclose, quotas) and what its own control
  may send; everything is off by default. Questions and delegation land in the destination's inbox for its control,
  which accepts (optionally creating its own task), declines or replies; knowledge requests go to a support session of
  the destination's KB. Replies are reduced to the destination's disclosure level before they are stored. Requests carry
  correlation, deadlines, cancellation, depth and the spaces their chain visited (loops and over-deep chains are
  refused) and are bounded per pair and per source. Only control runs have the eight `spaceman_xspace_*` tools. New
  **Cross-space** section per space (requests and access per peer) and a global **Cross-space** page; `xspace:read` /
  `xspace:write` permissions; events `xspace.request.created` and `xspace.request.state_changed` (metadata only).
- Revocation reaches open streams at once (SM-AUTH-007, ADR-0033): signing out, revoking a session or API token,
  rotating the root token, disabling a user, resetting a password or changing grants ends affected live terminals and
  change-feed streams as soon as the change commits. A removed space grant hides that space's changes immediately and
  ends its terminals with "Access lost". Expiry, idle timeout and changes made while central was stopped are caught by
  a periodic re-check (`auth.stream_revalidate`, default 30 s), which now also re-checks node connections' credentials.
  The web UI returns to sign-in when its session is gone and reconnects when it was only rotated.

### Changed
- `POST /api/v1/auth/logout` returns 200 with `{provider_logout_url}` instead of 204.
- Deleting central's copy of a recording (`DELETE /spaces/{id}/runs/{run}/recording`) needs the run to have ended
  (409 otherwise) and is now audited (`recording.deleted`) and recorded in the change feed.
- Backup verification compares the audit chain's head sequence and hash with the manifest, not the event count, so
  backups taken after audit retention verify.
- `auth.stream_revalidate` accepts at most 60 s (was 5 min).
- Launches can now be refused with `rate_limited`: a space runs at most `limits.max_concurrent_runs` worker and diagnostic
  runs (default 8), and the node's reported `capacity` is enforced. Control, KB support and dreamer runs have their own pools.
- KB support sessions can no longer ask knowledge bases (support depth 1 by default), so support never starts more support.
- Event deliveries record their origin (live, redelivery, replay, reprocess) and attempt; replays are recorded in a replay
  history and items they create are marked.

### Fixed
- Relocation's flush could report success after its context was cancelled.
- A space's lease guard no longer outlives its runs (SM-ARCH-004, ADR-0043). The node agent kept one
  `spaceman-agent lease-guard` process per space that had ever run, reading the lease file once a second while the space
  sat idle. The guard now ends with the space's last live run, and the next launch starts a new one before its run.
  Enforcement for live runs is unchanged.
- The Architecture page no longer says recordings, attachments and transcripts can be kept in S3. They are stored on
  central's local disk; only backups have an S3 target.
- Central no longer refuses to start (or to run `spaceman backup`) when the command-line client's (`SPACEMAN_SERVER`,
  `SPACEMAN_TOKEN_FILE`, `SPACEMAN_OUTPUT`, `SPACEMAN_SPACE`, `SPACEMAN_ALLOW_INSECURE_HTTP`), enrollment's
  (`SPACEMAN_ENROLL_TOKEN`) or the Terraform provider's (`SPACEMAN_ENDPOINT`) variables are set; they were rejected as
  unknown configuration overrides. `SPACEMAN_ALLOW_NETWORK_FS=1` (development mode only) now takes effect for the same reason.
- Documentation brought in line with the code: harness accounts exist (try-locally), inbox item sources, the
  concurrent-runs and storage-limit names, what the dev stack starts, backups without `git` leave role memory out
  instead of failing, what `/readyz` checks, backup and restore commands run as the service user with the systemd
  configuration path, root-token recovery with `spaceman admin root-token rotate`, lease-guard evidence (macOS only),
  and the Launch run and Add dream form fields. Accepted-but-unused settings (`auth.local_login_disabled`,
  `nodes.command_timeout`, the agent's `management_listen` and `harness.git_binary`) and the unimplemented S3 artifact
  store are stated in the configuration reference and the known limitations.
- Stale texts: the `spaceman_human_ask` tool no longer calls approvals "advisory for now", the root-token output names
  the Account page, and the corrupt-database error points at the backup guide.
- Documentation: the Porch setup now says to add `groups` to the application's allowed scopes. Without it Porch drops
  the scope and SpaceMan sees no groups (found by the real Porch suite).
- Recordings on a full disk: the run continues as before. Central's copy now ends with a `trunc` record (`disk`) and
  the run's close record instead of stopping silently. A quota truncation also reaches central's copy, and a recording
  resumed after a local loss continues above central's sequence numbers.
- A stopped node agent ends its recorders. In-process restarts used to leave two recorders on one run.
- A storage-busy API error (503 `unavailable`) now carries `Retry-After`.
- `make test-pulse` ran a directory that did not exist. It now runs the Pulse/NATS integration tests. New
  `make test-fault` suite (`test/fault`).
- Two tests that failed under heavy machine load (`TestRunKickAndRetryTimer`, `TestLoginThrottlingAndEnumeration`) now
  wait for the right condition and check that the dummy hash runs, without weakening what they assert.
- Live updates: a browser whose change-feed position is ahead of the server (after a restore from backup) now gets a gap
  and reloads instead of silently skipping changes.
- A space's goals and standing instructions now reach every run's system prompt (the launch dialog already said so).
- Accessibility (WCAG 2.1 AA): contrast of dimmed text, accent colors and badges in both themes; named close buttons,
  account menu, replay slider and task-list checkboxes; space sections are navigation links; the docs site's search
  button, links and code colors.

### Added
- Limits and budgets (SM-LIMIT-001/002, ADR-0036): one limits model with installation defaults (`limits:`), per-space
  overrides and a single effective read with sources (API and the space's **Limits** page). Concurrency, tool calls per
  run, event ingestion per subscription, KB support sessions, pending human actions, storage, task deadlines and
  usage-based token and cost budgets. Lowering a limit never stops running work unless the space chooses `on_lower: stop`.
  Budgets warn and stop new runs; they are not a hard currency limit, because usage is reported late.
- Schedules (SM-LIMIT-003): inbox items or tasks at cron or interval occurrences in an IANA time zone, with skip, run-once
  or bounded catch-up for missed runs and per-occurrence deduplication, driven by the single deadline timer (the space's
  **Schedules** page and API).
- Explicit replay of a subscription with a reason (`POST …/subscriptions/{id}/replay`), its history, and an option to
  create fresh items (SM-EVT-015).
- `spaceman` central: SQLite storage with migrations, keyring, hash-chained audit log, metrics and health endpoints, and the
  administrative HTTP API (OpenAPI 3.1, RFC 9457 problems, `If-Match`, idempotency keys, cursor pagination, live change feed).
- Access control: root-token bootstrap, Argon2id passwords, mandatory TOTP for privileged accounts, recovery codes,
  step-up re-authentication, password reset tokens, break-glass recovery with an acknowledgement alert, sessions with CSRF
  protection, viewer/operator/admin roles per space or globally (Casbin), and scoped API tokens.
- Nodes: one-time enrollment, node credentials, an outbound WebSocket protocol (`spaceman.node/v1`) with capability
  inventory, execution leases with generation fencing, disconnect enforcement and a lease-guard terminal for dead daemons.
- `spaceman-agent`: herdr-core runner over the framed protocol, idempotent launch and adoption, restart reconciliation,
  Claude Code and Codex adapters with isolated configuration directories, a launcher shim that keeps secrets out of
  herdr-core state, run credentials and a minimal MCP bridge.
- Spaces pinned to a node with goals, standing instructions, pause modes (hold, interrupt, emergency) and archive.
- Terminal recording into zstd segments, shipped to central, with a live watch-only stream, replay and download.
- Web UI (Mantine, Night Owl and Light Owl themes): setup, sign-in, overview, spaces, runs with live and replayed
  terminals, nodes, users, API tokens, audit log and account pages.
- Attachments: files uploaded to a space with sniffed types, size and quota limits, safe serving and garbage collection.
- Inbox per space: operator items in Markdown or JSON with attachments, urgency, statuses (in progress, deferred, rejected,
  done), deduplication by source key, and a TipTap editor that accepts pasted, dropped or attached images and files.
- Pulse events: connections with encrypted credentials, a catalog of versioned event templates (built-in GitHub issue and
  pull request templates), space subscriptions with one durable consumer each, explicit cursors (start now, replay),
  delivery history, quarantine with reprocessing, and template testing with sample events.
- Execution queue per space: one explicit order (priority on arrival, operators after), incident policy, dependencies with
  cycle detection, explicit blockers, exclusive resources, an active-task limit, selection with recorded skip reasons, and
  drag or keyboard reordering. Inbox items can become tasks.
- Human actions: questions, approvals (advisory for now), to-dos and messages for people, separate from the inbox and the
  queue, with type-specific responses, claiming, comments, withdrawal, expiry and follow-ups. An action can block its
  task until someone responds. A "Needs you" page lists pending actions across the spaces you can read.
- Accounts: Claude subscription tokens, Anthropic and OpenAI API keys stored sealed and never shown again, allowed per
  space with a default per harness. Launches pick the account deterministically and refuse clearly when none fits;
  rotation applies to later launches.
- Agent tools: every run's SpaceMan MCP server now serves the tools its kind may use, over the node connection: reading
  and adding inbox items, reading and managing tasks, fetching attachments, and asking people through human actions.
- Messages: durable messages and requests between agents, control and operators, with delivery states SpaceMan tracks.
  Idle agents get a one-line notice in their terminal; content is read through their tools. Human responses reach the
  agent that asked the same way. Each space has a Messages tab.
- Control and workers: control starts and stops workers for eligible tasks through its tools, each writer in its own git
  worktree (never reset or deleted). Control is notified of new inbox items, tasks awaiting review and ended workers,
  and is restarted with its session resumed if it stops unexpectedly, at most three times an hour.
- Roles and prompts: versioned prompts (drafts, publish, extend, fork, diff, rollback), harness profiles, role templates
  and space roles. Launches resolve them in a documented order with a permission-mode ceiling, and each run records its
  configuration in a manifest.
- Usage and transcripts: runs report token usage read from the harness's own records (Claude Code transcripts), with
  the harness's cost estimate and its assumptions; unknown usage is shown as unknown. Ended runs upload their native
  transcript, which can be downloaded or deleted.
- Dreams: a dreamer agent consolidates role memory from new transcript segments on demand or on a schedule, within
  time and evidence budgets; it reports findings, and the evidence it read counts as done only after its memory
  changes are checkpointed. A built-in prompt describes the method.
- Knowledge bases: agents ask a space's KB repositories through their tools; each requester gets its own support
  session (queued at capacity, put to sleep when idle, recovered when it fails); answers cite evidence and say whether
  they rest on accepted knowledge; proposals are accepted on the node after the KB's check, never forced.
- Role memory: each role's notes are Git history on the node, shared by its instances, checkpointed at idle points and
  run ends, merged without overwriting (conflicts are preserved and reported) and preserved centrally by fast-forward
  only. Spaces have a Memory section.
- Enforced approvals: policies make built-in agent tools or gateway tools wait for a person's approval, bound to the
  exact arguments, the requesting run, an expiry and a number of uses; the decision and each execution are recorded
  separately, and the approver's authorization is checked again before execution.
- MCP gateway: administrators register external MCP servers with sealed headers and grant them to space roles. Runs
  see their tools as `<server>__<tool>`, each run with its own upstream session; calls are rechecked, bounded, audited
  and dialed only to addresses the egress policy allows.
- Skills: a central catalog of versioned skills (SKILL.md plus reference files), including a built-in HallPass skill,
  assigned through role templates and space roles and written into each Claude Code run's config folder.
- Custom human-action types: administrators define request and response schemas, outcomes and form hints; spaces enable
  them; people answer through generated forms (or JSON) and agents discover them through their tools.
- Single sign-on: OpenID Connect providers such as Porch, with PKCE (S256), validated ID tokens, sealed write-only
  client secrets and refused private-network providers unless allowed. Group mappings (including Porch's
  `porch.groups`) grant a role in every space or one space, re-applied at every sign-in and only to SSO sessions.
  Pre-provisioned or just-in-time users, identity linking from the Account page, by an administrator or with a link
  invitation, step-up at the provider (or with the SpaceMan TOTP code where allowed), test logins, and a Sign-in
  providers page.
- Publishing SpaceMan's own events to Pulse (off by default, `events.outbound`): run, task, inbox item, human action,
  approval, memory and knowledge-base lifecycle events with versioned, metadata-only payloads, written in the same
  transaction as the change and relayed after commit with stable IDs, retries with backoff, dead-lettering, retention,
  correlation, causation and origin headers. SpaceMan never re-ingests its own events. Publication status (connection,
  backlog, lag, failures) on the Events page for administrators, in the API and as Prometheus metrics.
- Backups and restore: sealed backups (every file encrypted under the KEK, which is never included) of the database,
  role memory (Git bundles), recordings, attachments, transcripts and the effective configuration, with a manifest
  stating what is included and excluded, per-file SHA-256 and a sealed copy of the manifest. Local or S3-compatible
  targets, a schedule with one timer, retention by count and age, verification that opens every file and checks the
  database copy and audit chain, a Backups page, `/api/v1/system/backups` and `spaceman backup create|list|verify|restore`.
  A restore comes up with every space paused and execution authority renewed (lease generations advanced, earlier runs
  fenced, commands cancelled, run credentials revoked), event subscriptions replaying from their oldest stored delivery,
  approvals with uses left revoked and pending ones kept, and a restore notice until the spaces are resumed.
- Human-action follow-up: policies per action type with space overrides (Needs you shows reminded and escalated
  actions; **Policies** on a space's Human actions tab, installation defaults on Action types) for reminders, escalation
  (raise the priority, assign someone; the order never changes), rate limits per agent run and per space, and what
  happens to a response when the requester's run has ended: rerouted at once to a running successor-role run, else to
  control, else shown as undeliverable to people (with a message to operators and **Send to control**), or kept for the
  next run of the same agent instance until a hold ends. Routing is durable across restarts and records the original
  run, the previous target and the reason; responses are never shown to unrelated runs. Expiry, reminders, escalation,
  holds and approval expiry share one timer for the earliest deadline (the once-a-minute expiry job is gone). New
  outbound events `human.action.reminded`, `escalated` and `rerouted`; API under `/api/v1/human-policies`,
  `/api/v1/spaces/{id}/human-policies` and `…/notifications/{id}/route`.
- Packaging (Linux):
  - Reproducible release archives for linux/amd64 and linux/arm64 (`make release-snapshot`). Each holds `spaceman` with
    the UI embedded, `spaceman-agent`, systemd units, example configs and the licence files. They come with SPDX SBOMs
    (when syft is installed) and `SHA256SUMS`.
  - A hardened `spaceman.service`, and a `spaceman-agent.service` whose relaxations for harnesses are explained in
    ADR-0034.
  - A central container image (Debian slim with git), a node agent base image that is labelled and documented as
    **not agent-ready** (no herdr-core, no harnesses), and an example for building an agent runtime image from
    operator-supplied binaries.
  - A central-only Compose file, a container smoke test (`make image-smoke`), and a tag-triggered release workflow
    that only drafts a GitHub release and pushes no images.
  - Installation, containers (with the image matrix) and upgrade pages.
- Terraform provider (now in its own repository, `terraform-provider-spaceman`; phase 1, not published): 20 resources for spaces, space roles and
  account bindings, accounts, harness profiles, role templates, prompts, skills, MCP servers and grants, event
  connections, templates and subscriptions, human-action types and their space enablement, approval policies, knowledge
  bases, dream configurations, OIDC providers and group mappings, plus node, space and event-template data sources.
  Credentials are write-only arguments with a version to rotate them; updates send `If-Match` with the version from
  private state; catalog changes publish exactly one immutable version; UI edits show as drift only for configured fields;
  a `node_id` change is refused until relocation exists. Acceptance tests run against a throwaway local central
  (`make acc` in the provider repository).
- Documentation site (VitePress with Mermaid), real screenshots captured by `make screenshots`, a local development stack
  (`scripts/dev-stack.sh`) and a browser end-to-end journey (`make e2e`).
- Product contract (`docs/GOAL.md`), architectural decisions, execution plan, requirement traceability matrix and verified dependency
  contracts for herdr-core, Pulse, go-pulse-github, Porch, Hallpass, Claude Code and Codex.

[Unreleased]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.14...HEAD
[0.1.14]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.13...v0.1.14
[0.1.13]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.10...v0.1.13
[0.1.10]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.9...v0.1.10
[0.1.9]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.7...v0.1.9
[0.1.7]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.6...v0.1.7
[0.1.6]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.5...v0.1.6
[0.1.5]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.4...v0.1.5
[0.1.4]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.3...v0.1.4
[0.1.3]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/aurimasniekis/go-spaceman/compare/v0.1.0-rc.9...v0.1.0
[0.1.0-rc.9]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.9
[0.1.0-rc.8]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.8
[0.1.0-rc.7]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.7
[0.1.0-rc.6]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.6
[0.1.0-rc.5]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.5
[0.1.0-rc.4]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.4
[0.1.0-rc.3]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.3
[0.1.0-rc.2]: https://github.com/aurimasniekis/go-spaceman/releases/tag/v0.1.0-rc.2
