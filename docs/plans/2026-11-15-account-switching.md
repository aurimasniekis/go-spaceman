# Plan: switch a run's subscription account at its usage limit, and log usage per run, account, role and space

- **Status:** Planning only: no code changes. The operator answered Q1–Q12 on 2026-11-15 (§5); T0 applies them. Q4 and
  Q5 changed from the recommended defaults.
- **Epic:** when a run's Claude Code (and later Codex) subscription account reaches its usage limit, move the run to
  another account of its pool and continue the **same session**. Log usage (tokens, cost estimate, subscription
  windows) per run, account, role and space.
- **Constraint from the operator:** no proxy or gateway for model traffic (not Harbor Pilot, not `claude -p` behind a
  proxy). Runs stay the real interactive Claude Code with real subscription credentials; SpaceMan swaps the
  credentials. (Reading recorded as Q1.) This applies to subscription traffic; Harbor Pilot routing (ADR-0060) stays for 3rd-party/gateway models and is unchanged.
- **ADR:** **ADR-0090** in `docs/DECISIONS.md` (landed by T0 on 2026-11-15; the [draft file](2026-11-15-account-switching-adr.md)
  now only points there).
- **Builds on:** ADR-0079 (usage limits: detection, holds, keep-alive, park, resume), ADR-0009 (launcher shim),
  ADR-0021 (first-run seed), ADR-0053 (Codex ChatGPT logins), ADR-0068 (the PermissionRequest hook).

Evidence labels used below:
- **[code]**: read in this repository at `next/v0.1.6` (e4e3b56c).
- **[contract]**: already recorded in `docs/contracts/harnesses.md`.
- **[doc]**: Claude Code's official documentation (`code.claude.com/docs/en/…`) read on 2026-11-15.
- **[bin]**: strings of the installed Claude Code **2.1.325** binary on the node, not exercised.
- **[src]**: the `openai/codex` repository source (`codex-rs/`, HEAD of 2026-11-15; latest published npm version
  0.169.0). **The Codex CLI is not installed on the node**, so nothing about Codex was run.
- **UNVERIFIED**: inferred; a task below verifies it before anything relies on it.

---

## 1. What exists today

### 1.1 Usage limits (ADR-0079) [code]

- **Detection on the node** (`internal/agent/limit.go`): on OSC 7501 `idle`/`done`/`error` (2 s settle) and at run end,
  the agent tail-scans the run's transcript from a persisted offset and reads typed fields only (`apiError:
  "usage_limit_reached"`, `quotaLimits.{resetsAt,rateLimitType}`, the wrap-up meta record `usageLimitNote:
  "wrap_up"`, the gateway 429 prefix). Claude Code only: `limitScan` returns early for other harnesses.
- **Wire:** `RunReport.limit {kind, resets_at, since, cleared}` (protocol minor 12, `internal/proto/usagelimit.go`).
  `MaxMinor = 12`.
- **Central** (`internal/fleet/usagelimit*.go`, `limitwait.go`): a limit opens a **hold** (`usage_limits`, per space and
  account, read across spaces), the run is **kept alive** up to `usage_limit_keep_alive_max_minutes` (default 6 h)
  so Claude Code's own auto-continue can fire, then **parked**: stopped with a `limit_waits` row (session, task,
  launch). The `limit_waits` deadline job **resumes** a due wait as a new run of the same instance with `--resume
  <session>` and a fixed prompt; a failed resume gets one fresh-session retry.
- **Failover already exists, but only at launch and resume:** with the space limit `usage_limit_failover` (default
  off), `accounts.Resolve` skips bound accounts with an open hold (default first, then binding order) and records
  `resolved.account_failover {from,to,reason}` in the manifest. **It never moves a live run.** A resume rebuilds its
  launch from `launch_json` with the **old** `AccountRef`, so it fails over only when that account is still held.
- **No live run is ever moved to another account**, and no run-level switch history exists.
- **Notifications:** control messages per run window and per wait (`internal/control/usagelimit.go`), one human
  action per long hold (`usage-limit-human:<id>`), outbox events `fleet.usage_limit.reached|cleared`. No
  usage-limit-specific Telegram code: the human action reaches Telegram through the generic notifier.
- **UI:** `web/src/features/limits/UsageLimitsCard.tsx` (holds and waits, Resume now / Release / Clear) on the space's
  Limits page; the "at usage limit" run badge.
- **Retention:** `usage_limits` and `limit_waits` are never pruned.

### 1.2 Accounts and how credentials reach a run [code]

- `accounts` (`acc_…`): kinds `claude_oauth_token` (`claude setup-token`, delivered as `CLAUDE_CODE_OAUTH_TOKEN`),
  `anthropic_api_key`, `openai_api_key`, `codex_chatgpt_login` (an `auth.json`, ADR-0053). Secrets are sealed with
  `internal/keyring` (`PurposeCredential`, AAD `account:<id>`), write-only, never in manifests, events or logs.
- `space_accounts (space_id, account_id, harness, is_default)`: bindings per space and harness. **No priority, no
  role-level pool.** Harness profiles, role templates and space roles have no account field.
- At dispatch the hub opens the secret (`accounts.SecretEnv`) into `LaunchPayload.Secrets`; the node writes
  `<runDir>/secrets.json` (0600); the launcher shim (`spaceman-agent launch`) reads and deletes it, scrubs inherited
  `ANTHROPIC_*`/`CLAUDE_CODE_*`/`OPENAI_*`/`CODEX_*`, exports the run's secrets and `exec`s the harness (ADR-0009).
- Claude Code: `CLAUDE_CONFIG_DIR = <space>/instances/<instance>/claude`, one per instance. **SpaceMan never writes
  `.credentials.json`.** Sessions are `projects/<key>/<session>.jsonl` in that directory, so they belong to the
  instance, not the credential.
- Codex: `CODEX_HOME = <instance>/codex`; a ChatGPT login is one live `auth.json` per account and node under
  `<data>/logins/<hash>/`, symlinked into each `CODEX_HOME` (refresh tokens are single-use).
- **Session resume under another token** in the same `CLAUDE_CONFIG_DIR`: works against a fake API (2.1.323), the
  transcript records no account; **not verified with two real claude.ai accounts** (K-16) [contract].

### 1.3 Hooks, status line and usage logging today [code]

- SpaceMan installs one Claude Code hook: `PermissionRequest` (`spaceman-agent hook permission-request`). The
  profile's `hooks`, `statusLine` and other command keys are stripped unless SpaceMan or the profile owns them
  (`ClaudeUnsafeSettingsKeys`, OwnJSON).
- Usage: the node reads per-model token counts from the transcript (`internal/agent/usage.go`, 30 s debounce) and
  sends `usage.report`; central keeps one `run_usage` row per run (`models_json`, `estimate_json`) and gateway usage
  in `run_gateway_usage`. **No account or role column, no time series, no subscription-window data.** Codex usage is
  "unavailable".
- A run-scoped bridge socket (`internal/agent/bridge.go`) already authenticates a run's own processes by its run
  token (the MCP bridge uses it).

### 1.4 The two Harbor Pilot backlog items

- **"Harbor Pilot routing per space role / harness profile"** (backlog): route one role (e.g. a light role on a cheaper
  model) through a Harbor Pilot alias while other roles of the same harness keep their account. It is about **model
  routing for cost**, not about limits. This plan neither needs nor replaces it. Since the operator no longer wants
  the gateway for subscription traffic, Q10 asks whether to keep it. This applies to subscription traffic; Harbor Pilot routing (ADR-0060) stays for 3rd-party/gateway models and is unchanged.
- **"Gateway runs: pass the Harbor Pilot token as `CLAUDE_CODE_OAUTH_TOKEN`"** (backlog): only so Claude Code shows
  Harbor Pilot's usage headers on gateway runs. Direct runs already get `rate_limits` in the status line (§2.1), which
  this plan collects. **Recommendation: retire it** (Q10).

## 2. Verified facts the design rests on

### 2.1 Claude Code (2.1.325 installed)

| Fact | Source |
|---|---|
| `StopFailure` hook "fires instead of Stop when an API error (rate limit, auth failure, etc.) ended the turn"; fire-and-forget, output and exit code ignored. Input: common fields (`session_id`, `transcript_path`, `cwd`) + `error`, optional `error_details`, `last_assistant_message` (the rendered error text). | [doc] hooks, [bin] |
| `error` enum in the binary: `authentication_failed`, `oauth_org_not_allowed`, `account_on_hold`, `verification_required`, `billing_error`, `rate_limit`, `overloaded`, `invalid_request`, `model_not_found`, `server_error`, `unknown`, `max_output_tokens`, `cloud_credential_error` (`verification_required` is undocumented). | [bin], [doc] |
| The observed subscription-limit error records carry `error: "rate_limit"`, `apiError: "usage_limit_reached"`. So `StopFailure` with `rate_limit` fires on the **error path**; on the **wrap-up path** the turn ends normally and `StopFailure` does not fire (UNVERIFIED that it never fires there; inferred from the record shape). | [contract], inference |
| `Notification` hook types include `quota_auto_resume_fired`, `quota_auto_resume_stale`, `quota_auto_resume_disabled` (documented) and `…_armed`, `…_cancelled`, `…_offer`, `…_setting` (binary only). | [doc], [bin] |
| Status line: `statusLine {type:"command", command, refreshInterval?}`; runs at session start and resume, after each assistant message, on `/compact`, on a `rate_limits` window reset, on `refreshInterval` (≥ 1 s); 300 ms debounce, a newer run cancels an in-flight script. | [doc] statusline |
| Status-line JSON includes `session_id`, `transcript_path`, `version`, `model.id`, `cost.{total_cost_usd,total_duration_ms,total_api_duration_ms,total_lines_added,total_lines_removed}`, `context_window.{total_input_tokens,total_output_tokens,used_percentage,current_usage{…cache tokens}}`, and for claude.ai subscribers after the first response `rate_limits.five_hour.{used_percentage,resets_at}` and `rate_limits.seven_day.{…}` (each may be absent). `total_cost_usd` is a client-side list-price estimate. No account identity field. | [doc] statusline |
| Credential precedence: cloud provider env > `ANTHROPIC_AUTH_TOKEN` > `ANTHROPIC_API_KEY` > `apiKeyHelper` > `CLAUDE_CODE_OAUTH_TOKEN` > stored `/login` (`$CLAUDE_CONFIG_DIR/.credentials.json`). | [doc] authentication |
| **The env token cannot be swapped in a running process:** "Unless you run /login, Claude Code uses the token you set for the whole session. To replace an expired token, generate a new one and restart." | [doc] env-vars |
| The binary tracks `.credentials.json`'s mtime (`lastCredentialsMtimeMs`) and invalidates its cache when it changes, so a rewritten **stored** login is probably picked up mid-session. Trigger timing unknown; and the env token outranks it. UNVERIFIED. | [bin] |
| `apiKeyHelper` is re-run after its TTL (default 5 min, `CLAUDE_CODE_API_KEY_HELPER_TTL_MS`) and on any 401/403; its output goes as both `X-Api-Key` and `Bearer`. Whether a subscription OAuth token works through it: UNVERIFIED. A usage limit is a 429, which does not re-run the helper. | [doc] settings |
| `setup-token` tokens last one year and are subscription-only. | [doc] authentication |
| Interactive `claude` with only `CLAUDE_CODE_OAUTH_TOKEN` works once onboarding and folder trust are seeded. | K-10, ADR-0021 |
| A `/login` account switch cancels auto-continue (`account_switch`); a relaunch loses the armed timer, and a `--resume`d session needs a prompt to continue. | [contract], [doc] |
| Prompt caches are isolated between organizations, so the first request after a switch re-sends the context uncached. `SessionStart` on resume reports `prompt_cache_likely_expired` and `estimated_cache_write_usd`. | [doc] prompt-caching, hooks |
| Resuming one session in two processes interleaves writes into one transcript. | [doc] sessions |

### 2.2 Codex (source only, not run)

| Fact | Source |
|---|---|
| `auth.json` under `CODEX_HOME` (default `~/.codex`; the directory must exist); keys `auth_mode`, `OPENAI_API_KEY`, `tokens{id_token,access_token,refresh_token,account_id,…}`, `last_refresh`. `cli_auth_credentials_store`: `file` (default), `keyring`, `auto`, `ephemeral`. | [src] `login/src/auth/storage.rs`, `config/src/types.rs` |
| A running Codex caches auth in memory ("External modifications … will NOT be observed until `reload()`"). On a 401 it reloads `auth.json` **only if the on-disk `account_id` equals the running account** ("Skipping auth reload due to account id mismatch"), then refreshes. **So a different account cannot be swapped into a running Codex.** | [src] `login/src/auth/manager.rs` |
| Usage-limit error `UsageLimitReachedError {plan_type, resets_at, limit_window_minutes, rate_limits, rate_limit_reached_type, …}`; the error event is **not** written to the rollout. | [src] `protocol/src/error.rs`, `rollout/src/policy.rs` |
| `token_count` events **are** persisted to the rollout with `info` (token usage) and `rate_limits {primary, secondary: {used_percent, window_minutes, resets_at}, plan_type, rate_limit_reached_type}`. These are the typed signal for detection and usage. | [src] `protocol/src/protocol.rs`, `rollout/src/policy.rs` |
| `notify` program gets only `agent-turn-complete`; the hooks system (`SessionStart`, `Stop`, `PostModelRequest`, …) has no error or usage-limit event. | [src] |
| Sessions: `CODEX_HOME/sessions/YYYY/MM/DD/rollout-*.jsonl`; `codex resume <id>`. `session_meta` records `creator_account_id` and `account_scoped`; local history hides an account-scoped thread from another account. **Whether `codex resume <id>` refuses such a thread under another account: UNVERIFIED.** | [src] `protocol.rs`, `thread-store/src/local_history_account.rs` |
| Requests use `store: false` and resend the history from the local rollout; `previous_response_id` is an in-memory optimisation discarded when auth ownership changes. So server-side state does not bind the session to the account. | [src] `core/src/client.rs` |

## 3. Design

### 3.1 The shape of a switch: end the process, resume the session under the next account

Because the env token is fixed for the process (§2.1) and Codex refuses a foreign account in-process (§2.2), **a
switch is a restart**: SpaceMan ends the limited run and starts a **new run of the same instance**, with the same
session (`--resume <session>` / `codex resume <id>`), the next account of the pool, and a fixed prompt. This is
exactly ADR-0079's park-and-resume, with two changes: the wait is **due at once** and its launch **names the new
account**. Everything ADR-0079 built (fencing, a new run row per generation, the single-control index, task
re-pointing, the failed-resume fresh retry) is reused unchanged.

Rejected for v1 (kept as spike S-3, §6): an **in-place swap** by delivering subscription credentials as a stored
`.credentials.json` and rewriting it (the binary watches its mtime). It would keep background shells alive, but it
puts a long-lived token in the instance's config folder between runs, relies on undocumented behaviour, needs the
stored-login format for a `setup-token` token (no refresh token), and Claude Code treats a login change as an
`account_switch`. `apiKeyHelper` is rejected for the same reason plus: a 429 does not re-run it.

### 3.2 Account pools and the selection policy

- **Pool = the space's bindings for the harness**, ordered by a new `space_accounts.priority` (lower first; default
  100; the default account keeps its meaning for launches). **Role pools** (optional): `space_role_accounts
  (space_id, role, account_id, priority)`; when a role has any row, its pool is those accounts only (each must also be
  bound to the space for the harness). No role rows: the space pool.
- **Eligible:** enabled, bound, compatible kind, and **no hold** on it (any open `usage_limits` row whose reset hasn't
  passed, probe-due ones included, in any space: holds are read across spaces as today). An account a run left is
  held by construction, so a chain never returns to an exhausted account before its reset.
- **Order:** priority, then the lowest known utilisation of the window that limited the run (from §3.6's quota
  snapshot; unknown sorts last among equals), then fewest live runs on the account, then binding order.
  Deterministic for tests (the clock and snapshots are inputs).
- **Cool-down:** an account is ineligible until its hold clears (reset passed, evidence of progress, or an operator
  clear), as ADR-0079 decision 7 already enforces at launch. A hold with an unknown reset is skipped for switching
  even when its probe is due: a probe belongs to a resume or launch that has nowhere else to go.
- **Same policy for launches and resumes:** `Resolve` uses the pool order whenever `usage_limit_failover` is on, not
  only when the named account is held. A switch is "failover for a live run".
- **Settings** (space limits, so they get installation defaults, per-space overrides, audit and Terraform names):
  - `usage_limit_failover` (existing switch): now also enables live switching (Q2).
  - `usage_limit_wait_if_reset_within` (duration, default 15 min; Q4): when the limited account's reset is within this
    window, the run **waits** as ADR-0079 does (keep alive, then park) instead of switching. A reset further away
    switches. An unknown reset counts as further away.
  - `usage_limit_switch_max_per_day` (0–50, default 6): switches per instance per 24 h before the run falls back to
    ADR-0079 waiting; stops a switch storm (a forged record, a bad account).
  - `usage_limit_min_headroom_percent` (0–100, default 0 = off; Q5): a **launch-time headroom gate**. A new run (a
    launch, a fresh resume, or a switch's resume) starts on an account only if every window with known usage keeps at
    least this percentage free (`100 − used_pct`). An account with no known usage is not refused by this gate. When no
    eligible account passes, the start waits (the launch is refused with the reset time, as SM-RUN-005 does for an
    exhausted account). **There is no pre-emptive switch:** a running agent is never moved because of headroom.

### 3.3 Detection (Claude Code)

Primary stays ADR-0079's typed transcript scan; two accelerators and one new data source are added:

1. **Transcript scan (primary, unchanged):** still the only thing that opens a hold or sets a run's limit.
2. **`StopFailure` hook (accelerator):** SpaceMan owns a `StopFailure` hook,
   `spaceman-agent hook stop-failure`, that sends only the `error` enum value (allowlisted) and the session id over
   the run's bridge socket. The agent kicks the limit scan immediately (no 2 s settle) and records the class for
   non-limit failures (`authentication_failed`, `oauth_org_not_allowed`, `account_on_hold`, `billing_error`,
   `verification_required`) as a run health signal. `last_assistant_message` and `error_details` are **never read**
   (display text). The hook can't change anything in Claude Code; a missing or failing hook just leaves the scan.
3. **Status-line wrapper (new data):** SpaceMan owns `statusLine`, set to `spaceman-agent statusline`, with
   `refreshInterval` 60 s. It reads the JSON from stdin, forwards **allowlisted typed fields** (`rate_limits.*`,
   `cost.total_cost_usd`, `cost.total_api_duration_ms`, `context_window.used_percentage`, the token counts,
   `model.id`, `session_id`, `version`; the three strings are length-bounded and pattern-checked) over the bridge socket, and then, if the profile set its own `statusLine`,
   runs that command with the same stdin and prints its output (the "wrap the original status line" request);
   otherwise prints a short SpaceMan line. Bounded: 64 KiB of input, 2 s for the wrapped command, never blocks Claude
   Code on the socket.
   - This fills ADR-0079's biggest gap: **the wrap-up path has no reset time**; the status line's `rate_limits`
     window at ≥ 100 % gives `resets_at` (and its kind, `five_hour`/`seven_day`). The node attaches it to the limit
     report as `resets_at` with a new `source: statusline`, still clamped (8 days) and still only opening a hold
     together with a typed transcript record.
4. **Fallback:** a run whose limit is open but whose pool has no eligible account follows ADR-0079 unchanged
   (keep alive, park, resume at reset). A run on a node below the new protocol minor never switches (central drops
   the new fields, §3.9) and behaves as today.

### 3.4 The switch, step by step

Central decides; the node only stops and launches, as for any park and resume.

1. **Trigger.** A run report opens (or re-classifies) the run's limit (§3.3). The report transaction checks: failover on,
   the run kind parks (control, worker, diagnostic), the run had a turn (a session to resume), the limited account's
   reset is more than `usage_limit_wait_if_reset_within` away (Q4; an unknown reset counts as more), the instance's
   switch budget isn't spent, and `Resolve` finds an eligible account **other than** the run's. Otherwise, the run
   **waits**: ADR-0079 as today (keep alive, then park until the reset).
2. **Wait for an idle point.** The switch is armed (`account_switches` row, state `pending`) but the process is
   stopped only when the run is idle (OSC `idle`/`done`/`error` after the limit). A wrap-up limit lets the turn finish
   on its grace allowance; an error-path limit has already ended the turn. Armed for at most 10 min; a run still busy
   then is interrupted (as the park does).
3. **Park with an immediate resume.** In one transaction: stop the run (`desired = stopped`, interrupt), write the
   `limit_waits` row as `parkTx` does with `resume_at = now`, `switch_to_account_id = <next>`, link it from the
   switch row (`state = parked`). The hold on the old account opens as usual (it is what keeps the chain off it).
4. **Resume.** The `limit_waits` job picks it up at once (a parked switch is due immediately; it goes before
   reset-time resumes, control first). `waitLaunch` uses `switch_to_account_id` instead of `launch_json.AccountRef`;
   the launch transaction re-runs `Resolve` (the account may have been held meanwhile, or may fail the headroom gate of
   §3.2: pick the next, or fall back to waiting) and the usual checks. The new run gets `--resume <session>` and a fixed prompt: "SpaceMan moved this
   session to another subscription account because the previous one reached its usage limit. Continue the task; do
   not repeat completed work." The switch row moves to `resumed` with `new_run_id`.
5. **Verify.** The first assistant message with usage on the new run marks the switch `succeeded`. A resume that ends
   before its first turn gets ADR-0079's fresh retry (on the new account) and the switch is `failed`. An immediate
   limit on the new account is a new limit: another switch (budget permitting) or waiting.

What the node does: nothing new beyond the hook and status-line plumbing. The new account's token reaches the new
run through the existing secret path (secrets file → shim → env); the old token is not on disk.

**Codex in the same flow (stage C, §3.7):** the same steps with `codex resume <id>` and the ChatGPT login links
swapped to the new account's live `auth.json`.

### 3.5 What central tracks and shows

- **`account_switches`** (new table, `asw_` IDs): `id, space_id, instance_id, role, run_kind, from_run_id,
  new_run_id, from_account_id, to_account_id, harness, cause` (`usage_limit` | `operator`),
  `limit_kind, limit_resets_at, usage_limit_id, limit_wait_id, state` (`pending` | `parked` | `resumed` |
  `succeeded` | `failed` | `fell_back` | `cancelled`), `fell_back_reason` (fixed reasons), `created_at,
  updated_at, version`. Indexed by space and time, by account, by instance (for the daily budget).
- `cause` is `usage_limit` (the only automatic cause) or `operator` (forced by a person).
- **Audit:** `account.switch_started`, `account.switch_succeeded`, `account.switch_failed`, actor `system`
  (switch job), targets the run, details carry account IDs and the kind only. Operator-forced switches are audited
  with the user.
- **Outbox events:** `fleet.account_switch.started`, `fleet.account_switch.completed` (payload: IDs, kind, state).
- **Change feed:** resource `account_switch`.
- **Run timeline:** the run detail shows "Moved to account *B* (5-hour limit on *A*, resets 18:00 UTC) → run *r2*";
  the new run shows "Continued from run *r1* on account *A*". The chain is queryable through the switch rows.
- **UI:** UsageLimitsCard gains a "Switches" list (recent, with state); the Accounts page shows each account's live
  runs, holds and window utilisation (§3.6); space settings show pool order and the role pools (drag to order).
- **Control:** one message per switch, written in the switch transaction (dedup `account-switch:<asw id>`): "Run
  *r1* (role *x*) reached its usage limit on account *A*; continued as run *r2* on account *B*." It **replaces** the
  ADR-0079 "parked" and "resumed" messages for that wait. A fallback to waiting keeps ADR-0079's messages.
- **People / Telegram:** no message per switch (noise). People hear about it only when **the whole pool is
  exhausted** (§3.8): one human action per space and window, dedup `account-pool-exhausted:<space>:<window start>`.
  That reaches Telegram through the generic human-action notifier, as today (Q7).

### 3.6 Usage and token logging

**Collected** (per run, cumulative, from the node):
- Tokens per model from the transcript, as today (input, output, cache write, cache read).
- From the status line: `cost.total_cost_usd` (estimate, labelled as such), `total_api_duration_ms`, and the
  `rate_limits` windows (`used_percentage`, `resets_at`) of the run's account.
- Codex (stage C): `token_count.info` (input, cached input, output, reasoning) and `rate_limits` from the rollout.

**Wire:** `UsageReport` gains `windows: [{kind, used_pct, resets_at, observed_at}]` and `cost.total_usd_estimate`;
the per-model counters stay. A new minor (§3.9).

**Stored:**

| Table | What | Retention |
|---|---|---|
| `run_usage` (existing) | + `account_id`, `role`, `harness` (backfilled from `launch_json` in the migration) | with the run, as today |
| `usage_buckets` (new) | `(space_id, account_id, role, harness, model, bucket_start)` hourly; sums of the four token counts, reasoning tokens, cost estimate, turns. Written in the usage-report transaction from the **delta** against the run's stored cumulative counters (cumulative reports make repeats harmless). | hourly rows 90 days, then rolled into daily rows kept 730 days (space-level overrides as the other retention limits) |
| `account_windows` (new) | latest snapshot per `(account_id, kind)`: `used_pct, resets_at, observed_at, run_id` (the newest observation wins; an older `observed_at` is ignored) | current state only |
| `account_window_samples` (new) | at most one sample per account and window per 10 min, for charts | 30 days |

Accounts are global, so account-level rows carry no space; the space views filter by `space_id` (buckets) or by the
space's bindings (windows). Retention runs in `internal/retention` like the other prunes (new `Kind` constants,
batched deletes). Also new: prune `usage_limits`, `limit_waits` and `account_switches` rows older than 180 days that
are closed (they are never pruned today).

**API:**
- `GET /spaces/{id}/usage?from&to&bucket=hour|day&group_by=role|account|model|harness` (`run:read`)
- `GET /accounts/{id}/usage?from&to&bucket&group_by=space|role|model` (`account:read`)
- `GET /accounts/{id}/windows` and `GET /spaces/{id}/account-windows` (the bound accounts' latest windows)
- `GET /spaces/{id}/account-switches`, `GET /runs/{id}` gains `account_id` and the switch links
- CLI: `spaceman usage space|account`, `spaceman accounts windows`; Telegram classes: read-only ops allowed.

**UI:** a space **Usage** page (tokens and estimated cost over time, stacked by role or account; a table per role),
an account panel with 5-hour and 7-day bars and reset times, and a run's own usage (exists; add account and cost).

**Metrics** (bounded labels): `spaceman_account_switches_total{harness,cause,outcome}`,
`spaceman_account_pool_exhausted_total{harness}`. No account or space label (unbounded).

### 3.7 Codex: staged

Codex parity needs facts that can't be checked on this node (no Codex CLI, no ChatGPT accounts):
- **C1 spike:** install a pinned Codex CLI on the node, and with two ChatGPT logins verify `codex resume <id>` of
  an `account_scoped` thread under the other account, the `token_count.rate_limits` shape in a real rollout, and what
  the rollout shows at a real limit.
- **C2 detection:** extend `limitScan` to Codex rollouts: a limit is a `token_count` whose `rate_limits` has
  `rate_limit_reached_type` set or a window at `used_percent ≥ 100`, with that window's `resets_at`; cleared by a
  later `token_count` below 100 % or a turn with usage. Typed fields only. This also gives Codex the ADR-0079 holds,
  keep-alive and park it lacks today.
- **C3 switch:** the §3.4 flow with `codex resume`; the node swaps the instance's login links to the new account. If
  C1 shows the account filter refuses the resume, the switch starts a **fresh** session with the original prompt and
  a summary note (documented as lossy), or Codex switching stays off (decided after C1, Q8).
- **C4 usage:** Codex token counts and windows into §3.6.

### 3.8 Failure modes

| Case | Behaviour |
|---|---|
| **Every pool account exhausted** | ADR-0079 unchanged: keep alive or park. The wait's resume time becomes the **earliest reset among the pool** (not only the run's account); at resume, `Resolve` picks whichever account freed up. Control is told once per space and window ("account pool exhausted until …"); people get one human action when the earliest reset is more than `usage_limit_notify_after_hours` away. |
| **Limit mid tool call** | The limit hits on an API request, so no tool is executing in the foreground. Background shells and subagents end with the process (as with a park today); the transcript keeps everything finished. The switch waits for an idle point (§3.4 step 2), at most 10 min. The resume prompt tells the agent to check for unfinished work. |
| **Concurrent runs on one account** | Each run switches on its own detection; the first one's hold makes launches and other switches avoid the account. Runs still working on it aren't interrupted pre-emptively (their requests will fail at the same limit and they switch then). The "fewest live runs" tie-break spreads switched runs across the pool. |
| **Two runs pick the same next account** | Allowed; the second limit (if any) is handled as any limit. No reservation (subscription limits are per account, not per run). |
| **Switch storm** (forged records, an account that is always limited, flapping) | `usage_limit_switch_max_per_day` per instance; a forged record can hold an account for at most 8 days (ADR-0079 clamp) and is cleared by any progress on it. |
| **New account fails auth** (`authentication_failed`, `oauth_org_not_allowed`, `account_on_hold`) | Not a usage limit: no hold, no further switch. The run ends `failed` as today; the StopFailure class is recorded on the switch (`failed`, reason `auth`) and control is told; the account is flagged "needs attention" in the UI (Q6). |
| **Account deleted/disabled/unbound between arm and resume** | The launch's `Resolve` skips it and picks the next; none left → wait. |
| **Node offline or restarting during a switch** | The `limit_waits` row is durable; the job retries on lease (existing). A switch row stays `parked` until then. |
| **Control's own limit** | Control is switched like workers (it parks today). Control isn't told about its own switch; the next control run's prompt says so. |
| **Node below the new minor** | No hook or status-line data; central treats limits as ADR-0079 does, and the switch still works through the transcript scan (the switch itself needs no new node capability: it is a park and a launch). |

### 3.9 Protocol and migration impact

- **Protocol minor 13** (`MinorAccountUsage`): `UsageReport.windows`, `UsageReport.cost.total_usd_estimate`,
  `RunLimit.source` (`transcript` | `statusline`), `RunReport.failure` (the StopFailure class, allowlisted). All
  optional, stripped below 13 by `ForMinor`. **No new command:** a switch is a stop and a launch. Launch payloads are
  unchanged (the account is already a launch input).
- **Central migration 0054** (number at merge time): `space_accounts.priority`; `space_role_accounts`;
  `account_switches`; `limit_waits.switch_to_account_id`, `limit_waits.account_switch_id`; `run_usage.account_id`,
  `role`, `harness` (backfill); `usage_buckets`; `account_windows`; `account_window_samples`. `usage_limits.source`
  has a CHECK from 0050, so adding `statusline` there would need a table rebuild: **don't**. The status line only
  fills `resets_at` on a limit the transcript opened; the hold's source stays `transcript`.
- **Agent migration 0009:** none needed for the switch; the bridge-socket intake keeps the latest status-line snapshot
  in memory and the usage debounce sends it.
- **Settings:** `statusLine` and the `StopFailure` hook become SpaceMan-owned keys in `settings.json` (OwnJSON), with
  the profile's own status line wrapped and its own hooks merged. The protected-files lists stay as they are.
- **GOAL changes (ADR):** SM-ACCT-003's "No automatic rotation or budget arbitration" is amended; new SM-ACCT-007
  (pools), SM-RUN-007 (a usage limit moves a run to another account of its pool when one is eligible),
  SM-OBS-007 (usage ledger with retention). SM-RUN-004..006 stay.

### 3.10 Secrets

- No new secret storage: tokens stay sealed in `accounts` and travel only in the launch path (ADR-0009). A switch never
  sends a token to a running process and never writes one to the instance folder.
- The hook and status-line commands forward **allowlisted typed fields only**; no message text, path content, cwd or
  PR URL leaves the node. They authenticate with the run token on the bridge socket, so one run can't report for
  another.
- Logs, events, audit details and switch rows carry account IDs, never secrets or emails. The account picker in the
  UI shows names only.
- Codex logins keep ADR-0053's one-live-file-per-account rule; a switch swaps symlinks, never copies a login.

## 4. Tasks

Tiers: **light** (docs, small UI, plumbing with tests), **medium** (a feature across layers), **hard** (the risky
core). Every task: `make check-fast` green, its own tests, `make web-check`/`site-check`/`e2e` when affected, a PR
into the integration branch `feat/account-switching`, a reviewer before done.

| # | Task | Tier | Depends on |
|---|---|---|---|
| T0 | ADR + GOAL amendments | light | Q answers |
| T1 | Spike: two real claude.ai accounts | medium | — |
| T2 | Pools: priority and role pools | medium | T0 |
| T3 | Switch core | **hard** | T0, T2 |
| T4 | Hook + status-line intake on the node | medium | T0 |
| T5 | Protocol minor 13 + central intake of windows and failures | medium | T4 |
| T6 | Usage ledger: buckets, windows, retention | medium | T5 |
| T7 | Usage API + CLI | light | T6 |
| T8 | Web: switches, pools, windows, usage page | medium | T3, T7 |
| T9 | Launch-time headroom gate (no pre-emptive switch) | medium | T2, T6 |
| T10 | Pool-exhausted handling and notices | medium | T3 |
| T11 | Terraform attributes and docs | light | T2, T3, T9 |
| T12 | Integration e2e and the real checklist | medium | T3, T5, T8, T10 |
| C1 | Codex spike | medium | Codex CLI on the node |
| C2 | Codex limit detection | medium | C1 |
| C3 | Codex switch | **hard** | C2, T3 |
| C4 | Codex usage | light | C1, T6 |
| H1 | Retire the gateway-token backlog item; re-scope routing | light | Q10 |

### T0 — ADR and GOAL amendments (light, docs only)
Land the draft ADR in `docs/DECISIONS.md` with the next free number and the operator's answers; amend SM-ACCT-003,
add SM-ACCT-007, SM-RUN-007, SM-OBS-007 to GOAL.md and TRACEABILITY rows (not implemented); add K-17 (real two-account
evidence). **Accept:** ADR labelled security-relevant; requirement IDs added, none renumbered; `make site-check`.

### T1 — Spike: switching between two real claude.ai accounts (medium)
Needs two operator-provided subscription tokens (handed over out of band, never committed). With an isolated
`CLAUDE_CONFIG_DIR`: start interactive on token A, finish a turn, end the process, `--resume` on token B; check that
the conversation continues, what `.claude.json` (`oauthAccount`) does, whether a status line shows B's
`rate_limits`, and whether `StopFailure`/`Notification` fire as expected (at a real limit if one can be reached
cheaply; otherwise record as unverified). Record in `docs/contracts/harnesses.md` with versions; resolve or narrow
K-16. **Accept:** contract section with labelled evidence; no credential in the repo, logs or attachments.

### T2 — Pools: priority and role pools (medium)
Migration: `space_accounts.priority`, `space_role_accounts`. `accounts.Resolve` takes the role and orders by the §3.2
policy (priority, utilisation when known, live runs, binding order) behind `usage_limit_failover`; API/CLI for
priority and role pools (`If-Match`), audit `space.accounts_bound` details. **Accept:** table-driven Resolve tests
(holds, disabled, role pool vs space pool, ties); API round-trip; default behaviour unchanged with failover off.

### T3 — Switch core (**hard**)
`account_switches` table and IDs; arm in the report transaction, only when the limited account's reset is more than
`usage_limit_wait_if_reset_within` away (Q4); idle-point stop (10 min cap); park with
`resume_at = now` and `switch_to_account_id`; `waitLaunch` uses it and re-resolves (headroom gate included); state
machine to `succeeded`/`failed`/`fell_back`; daily switch budget; control message `account-switch:<id>` replacing
park/resume messages; audit, outbox, change feed; metrics. **Accept:** fleet tests for each state and fallback (no
eligible account, budget spent, account deleted, resume failed, node offline, new account limited at once, control
run); tests for the near-reset wait (reset inside the window waits as ADR-0079; outside it switches; unknown reset
switches);
an app e2e with the fake harness driving a limit record → a new run on account B with `--resume` of the same
session; race run of `internal/fleet`; ADR-0079 tests unchanged.

### T4 — Hook and status-line intake on the node (medium)
`spaceman-agent hook stop-failure` and `spaceman-agent statusline` (wrap the profile's status line, 64 KiB/2 s bounds,
allowlist); OwnJSON wiring in `Claude.Build`; bridge-socket intake authenticated by run token; StopFailure kicks the
limit scan; latest snapshot kept per run. String fields (`model.id`, `session_id`, `version`) are length-bounded and
pattern-checked. **Accept:** unit tests for the allowlist (no text field crosses), the bounds, the
wrapped command's output and timeout, a missing agent socket not breaking Claude Code; a real print-mode check
(opt-in `SPACEMAN_REAL=1`) that the status line command is invoked with the documented JSON.

### T5 — Protocol minor 13 and central intake (medium)
`MinorAccountUsage`, the optional fields, `ForMinor`; `RunLimit.source = statusline` fills a missing `resets_at` on a
wrap-up limit (clamped); `RunReport.failure` stored on the run and shown. **Accept:** proto round-trip and
downgrade tests; a minor-12 node keeps today's behaviour; fleet test for the filled reset.

### T6 — Usage ledger (medium)
Migration (`run_usage` columns + backfill, `usage_buckets`, `account_windows`, `account_window_samples`); delta
accounting in the usage-report transaction; retention prunes (buckets, samples, closed holds/waits/switches).
**Accept:** delta tests (repeats, out-of-order, final report, switch chain); retention test; migration test from
0053.

### T7 — Usage API and CLI (light)
The §3.6 endpoints, `make api`, CLI commands, Telegram classes (read-only). **Accept:** authz tests (space vs
account permission), API docs regenerated.

### T8 — Web (medium)
Switches list and run timeline links; pool order and role pools in space settings; account windows panel; space
Usage page (dataviz conventions). **Accept:** vitest + Playwright, axe clean, screenshots updated.

### T9 — Launch-time headroom gate (medium; replaces the pre-emptive switch, Q5)
`usage_limit_min_headroom_percent` (default 0 = off). `accounts.Resolve` (T2) skips an account for a launch, a fresh
resume or a switch's resume when a window with known usage (from `account_windows`, T6) leaves less than the
configured percentage free, and picks the next eligible account, or waits when none passes. **No pre-emptive switch:**
running agents are never moved for headroom. **Accept:** table-driven tests (boundary at the threshold; unknown usage
not skipped; every account below the threshold → wait with the reset time; a switch's resume is gated; 0 changes
nothing); off by default.

### T10 — Pool exhausted (medium)
Wait resume time = earliest pool reset; resume re-resolves; control message and one human action per space and window
(`account-pool-exhausted:…`, reserved dedup prefix). **Accept:** fleet + control tests; reserved-prefix test.

### T11 — Terraform and docs (light)
Provider attributes for the new limits, pool priority and role pools; `site/guide/usage-limits.md` (switching,
pools, usage page), LIMITATIONS, THREAT_MODEL row. **Accept:** `make site-check`; provider tests.

### T12 — Integration e2e and the real checklist (medium)
End-to-end with the fake harness: limit → switch → resumed session → usage buckets per account and role; a
pool-exhausted case; a real checklist for the operator (two accounts, one limited) recorded in TRACEABILITY as
external evidence when run. **Accept:** e2e green; checklist in `docs/`.

### C1–C4 — Codex (staged)
As §3.7. C1 needs the Codex CLI on the node and two ChatGPT logins. **Accept:** C1 contract section;
C2/C3/C4 like T3/T6 for Codex.

### H1 — Harbor Pilot backlog (light)
Per Q10: cancel the gateway-token item with a note pointing to this plan; keep or cancel the routing item.

## 5. Questions for the operator: answered 2026-11-15

Q1–Q3 and Q6–Q12 take the recommended default. Q4 and Q5 are changed (the answer column says how). The rest of the
plan applies these answers.

| # | Question | Recommended default | Answer (2026-11-15) |
|---|---|---|---|
| Q1 | We read "no hall pass / no `claude -p`" as: no proxy or gateway for model traffic; runs stay interactive Claude Code with real subscription tokens, and SpaceMan swaps the token by restarting the process and resuming the session. Correct? | Yes. | Accepted (default). |
| Q2 | Should the existing `usage_limit_failover` switch also turn on live switching, or a separate switch? | One switch (`usage_limit_failover`) for launches, resumes and live runs; still **off by default** per space. | Accepted (default). |
| Q3 | Pools: per space with an optional per-role pool, ordered by priority. Enough, or do you want per-harness-profile pools too? | Space pool + optional role pool; no profile pools. | Accepted (default). |
| Q4 | Switch the session immediately at the limit, even when the reset is close (e.g. 20 min away)? | Switch whenever an eligible account exists; the session continues at once. (Alternative: keep ADR-0079's keep-alive when the reset is under N minutes.) | **Changed: wait instead of switching when the reset is near.** New space setting `usage_limit_wait_if_reset_within`, default **15 min**. If the limited account resets within that window, the run waits (ADR-0079 park and wait). Otherwise it switches to the next eligible account. (§3.2, §3.4.) |
| Q5 | Pre-emptive switch before the limit (at an idle point, at ≥ X % of a window)? | Off by default (`100`); recommend 95 for spaces with a pool. | **Changed: no pre-emptive switch.** Replaced by a launch-time headroom gate, `usage_limit_min_headroom_percent` (per space, off by default; T9). A new run (launch or fresh resume, or a switch's resume) does not start on an account whose known window usage leaves less than X % free; it picks the next eligible account, or waits. Running agents are not switched pre-emptively. |
| Q6 | An account that fails authentication after a switch: stop, or try the next account? | Stop and tell control; flag the account. No automatic second switch on auth errors. | Accepted (default). |
| Q7 | Notifications per switch: control only, or also Telegram/people? | Control only; people (and so Telegram) only when the whole pool is exhausted. | Accepted (default). |
| Q8 | Codex: build after the Claude Code switch ships, and only after the C1 spike? If a Codex session can't be resumed under another account, accept a fresh session with a note, or keep Codex switching off? | Staged after Claude Code; if resume is refused, keep Codex switching off. | Accepted (default). |
| Q9 | Usage retention: hourly buckets 90 days, daily 730 days, window samples 30 days, closed holds/waits/switches 180 days? | Yes. | Accepted (default). |
| Q10 | Harbor Pilot backlog: retire "pass the HP token as `CLAUDE_CODE_OAUTH_TOKEN`"? Keep "HP routing per role" (it's about cheaper models, not limits)? | Retire the token item; keep routing in backlog until you decide on gateway use at all. | Accepted (default). This applies to subscription traffic; Harbor Pilot routing (ADR-0060) stays for 3rd-party/gateway models and is unchanged. |
| Q11 | T1 needs two real subscription tokens on the node for a short, budget-bounded spike. OK to provide them? | Yes, two tokens, revoked after the spike or kept as the pool. | Accepted (default). Tokens are handed over out of band, never committed (K-17). |
| Q12 | Cost: show the status line's `total_cost_usd` (a list-price estimate) on the usage page? | Yes, labelled "estimate (list price)"; tokens are the primary unit. | Accepted (default). |

## 6. Spikes and evidence still missing

- **S-1 (T1):** two real claude.ai accounts: `--resume` across accounts, `oauthAccount`, status-line `rate_limits`,
  StopFailure at a real limit.
- **S-2 (C1):** Codex: resume under another ChatGPT account, `token_count.rate_limits` in a real rollout.
- **S-3 (later, optional):** in-place swap through a stored `.credentials.json` (mtime re-read) to keep background
  shells across a switch. Only if restarts prove too disruptive; would need its own security-relevant ADR.
- Until T1 runs, the switch is **implemented and simulated**, not externally verified (AGENTS.md status rules).

## 7. Notes for implementors and reviewers

- Do not change ADR-0079's detection rules or its hold semantics; the switch sits on top of park/resume.
- No transcript or status-line **text** crosses the wire or reaches a log (SM-RUN-002, SM-OBS-001).
- No network or file I/O inside `store.Writer.Tx`; messages, events and audit go in the same transaction through the
  outbox and `NotifyControlTx`.
- New dedup prefixes (`account-switch:`, `account-pool-exhausted:`) go into `reservedDedupPrefixes`.
- New API ops need Telegram classification, CLI coverage and `make api` (see the repository's API checklist).
