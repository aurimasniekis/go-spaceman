# Draft ADR: a usage limit moves a run to another account of its pool (security-relevant change)

This is the draft that T0 of [the plan](2026-11-15-account-switching.md) copies into `docs/DECISIONS.md` once the
operator has answered the plan's questions. The number is provisional (**ADR-0090**, the next free number at the time
of writing); T0 uses the next free number when it lands.

---

### ADR-0090 — A usage limit moves a run to another account of its pool; usage is logged per run, account, role and space (security-relevant change)

- **Status:** Proposed, 2026-11-15. Not implemented. Depends on the operator's answers to the plan's Q1–Q12.
- **Security-relevant because:**
  - it amends SM-ACCT-003 ("No automatic rotation or budget arbitration");
  - content of a run's own transcript (untrusted) can now move the run to another credential;
  - SpaceMan takes ownership of two more Claude Code settings keys that run commands (`statusLine`, a `StopFailure`
    hook).
- **Context:**
  - ADR-0079 keeps a limited run alive or parks it until its reset. Its failover (`usage_limit_failover`, off by
    default) only applies when a run is launched or resumed, and a resume names its old account, so in practice a
    limited run waits.
  - The operator wants a limited run to continue at once on another subscription account of its pool, without a
    proxy or gateway (no Harbor Pilot, no `claude -p` behind a proxy).
  - Claude Code fixes `CLAUDE_CODE_OAUTH_TOKEN` for the life of the process ("To replace an expired token, generate a
    new one and restart", official env-vars docs). Codex refuses to adopt a different account's `auth.json` in a
    running process (`codex-rs/login/src/auth/manager.rs`: reload skipped on an account-id mismatch).
  - Sessions belong to the instance's config folder, not the credential. `--resume` under another token works against
    a fake API (2.1.323); with two real claude.ai accounts it is not verified (K-16).
  - Claude Code 2.1.325 has a `StopFailure` hook (fire-and-forget; `error` enum includes `rate_limit`,
    `authentication_failed`, `billing_error` …). Its status line receives `rate_limits.{five_hour,seven_day}.
    {used_percentage,resets_at}` for subscription accounts, plus cost and token fields (official statusline docs).
- **Decision:**
  1. **A switch is a park with an immediate resume on another account.** When a run's limit opens, failover is on, the
     run had a turn, and the pool has an eligible account other than the run's, central arms a switch. At the run's
     next idle point (at most 10 min) it parks the run as ADR-0079 does, with `resume_at = now` and the target
     account, and the resume launches a new run of the same instance with `--resume <session>` under that account and
     a fixed prompt. Nothing is sent to a running process; no credential is written to the instance folder.
  2. **Pools.** The space's bindings for the harness, ordered by `space_accounts.priority`; an optional role pool
     (`space_role_accounts`) replaces it for that role. Eligible: enabled, bound, compatible, no open hold in any
     space (probe-due holds included). Order: priority, lowest known window utilisation, fewest live runs, binding
     order. The same order applies to launches and resumes whenever failover is on.
  3. **Settings.** `usage_limit_failover` (existing, default off) enables live switching too.
     `usage_limit_switch_max_per_day` (default 6 per instance) bounds switches; beyond it, ADR-0079's wait applies.
     `usage_limit_switch_at_percent` (default 100 = off) arms a pre-emptive switch at an idle point.
  4. **Detection.** ADR-0079's typed transcript scan stays the only thing that opens a hold or sets a limit.
     SpaceMan-owned `StopFailure` hook and status-line wrapper report allowlisted typed fields over the run's
     authenticated bridge socket: the hook kicks the scan and records the failure class; the status line supplies
     window utilisation, resets (filling the wrap-up path's missing reset, clamped as before), cost estimate and
     tokens. A profile's own status line is wrapped (run with the same input, its output shown). No display text is
     read or forwarded.
  5. **History and notices.** `account_switches` records every switch (states pending → parked → resumed →
     succeeded | failed | fell_back | cancelled), with audit (`account.switch_*`), outbox events
     (`fleet.account_switch.*`) and the change feed. Control gets one message per switch
     (`account-switch:<id>`), replacing the park/resume messages of that wait. People are told only when the whole
     pool is exhausted (one human action per space and window).
  6. **All accounts exhausted.** ADR-0079 unchanged, except the wait's resume time is the earliest reset in the pool,
     and the resume re-resolves.
  7. **Usage ledger.** `run_usage` gains account, role and harness; `usage_buckets` (hourly, rolled to daily) holds
     token sums and the cost estimate per space, account, role, harness and model, written from cumulative deltas in
     the usage-report transaction; `account_windows` holds each account's latest window snapshot, with 10-minute
     samples. Retention: hourly 90 d, daily 730 d, samples 30 d; closed holds, waits and switches 180 d.
  8. **Codex** follows in stages after a spike with real ChatGPT logins: typed `token_count.rate_limits` detection,
     then the same switch with `codex resume` and the login links swapped. If a resume under another account is
     refused, Codex switching stays off.
  9. **Numbers.** Protocol minor 13 (`MinorAccountUsage`: usage windows, cost estimate, limit source, failure class;
     all optional). One central migration (pools, switches, ledger). GOAL: SM-ACCT-003 amended; SM-ACCT-007,
     SM-RUN-007, SM-OBS-007 added.
- **Alternatives:**
  - **Route through a gateway that holds several accounts** (Harbor Pilot). Rejected by the operator: usage, caching
    and behaviour differ from the real interactive client.
  - **In-place swap through a stored `.credentials.json`** (the binary re-reads it on an mtime change). Keeps
    background shells, but leaves a long-lived token in the instance folder between runs, relies on undocumented
    behaviour and needs a stored-login shape for `setup-token` tokens. Deferred to an optional spike.
  - **`apiKeyHelper`** printing the current token. A 429 does not re-run it, whether a subscription token works through
    it is unverified, and it outranks the env token for every run.
  - **Switch on the status line alone** (utilisation at 100 %). Rejected as the only signal: it is display-adjacent
    data from a command SpaceMan runs, not a typed transcript record; used only to fill resets and for the opt-in
    pre-emptive switch.
- **Consequences:**
  - A forged transcript record can now also move a run to another account. Bounded by the 8-day hold clamp, the
    daily switch budget, and the fact that the target is always an operator-bound account of the same pool.
  - A switch ends the process: background shells and subagents end, the prompt cache starts cold under the new
    organization (an extra uncached request).
  - Until two real accounts are tested (K-16, plan T1), the switch is implemented and simulated only.
