# Account switching: operator checklist (two real accounts, one limited)

> Status: **not yet run.** The automated tests (`internal/app` `TestAccountSwitchEndToEnd`,
> `TestAccountSwitchWaitsWhenResetNear`, `TestHeadroomGateEndToEnd`, `TestPoolExhaustionNoticedOnce`) use a scripted node
> and a fake clock. They show SpaceMan's logic, not that Claude Code behaves the way the logic assumes. This checklist is
> the evidence against the real thing. Until it has been run and recorded in
> [TRACEABILITY.md](TRACEABILITY.md) (SM-RUN-007, SM-OBS-007, SM-ACCT-007), the real evidence stays "none".

## What it proves

1. A run that reaches a real subscription limit is moved to the second account and **continues the same session** there
   (`--resume` of the same session ID under a different account's token works; the transcript continues).
2. The usage ledger books the two accounts' usage separately, per account and per role, and counts the resumed session
   once.
3. The rate-limit windows read from a real Claude Code status line are plausible and agree with what Claude Code itself
   shows.
4. The launch headroom gate skips an account that is nearly used and the pool-exhausted notice appears once.

Open items it closes or confirms: the real two-account switch and the first live status-line `rate_limits` payload (K-16,
K-17).

## Before you start

- A disposable space on a node you control, with one Claude Code harness profile and the harness version noted.
- **Two Claude subscription accounts** (setup-token accounts), A and B, that you may spend. Use accounts you can afford to
  exhaust for a few hours: the test needs A at its limit. Prefer a Pro account with a 5-hour window as A.
- Both accounts created in SpaceMan (write-only: never paste a token into a ticket, a log or this file) and bound to the
  space as Claude accounts, with A as the space's default and explicit priorities (A first, B second). Without a default
  an unnamed launch is refused:
  `spaceman account set-space --space <space> --account <A> --account <B> --default <A> --priority <A>=1 --priority <B>=2`.
  If the role has a role pool, both accounts must be in it.
- Space settings (see [Usage limits](../site/guide/usage-limits.md)): failover on, and leave
  `usage_limit_wait_if_reset_within` at its default (15 minutes) and `usage_limit_min_headroom_percent` at 0 for now:
  `spaceman limits set --space <space> --set usage_limit_failover=1 --set usage_limit_min_headroom_percent=0`.
  Optionally cap switches with `--set usage_limit_switch_max_per_day=<n>`. Verify with
  `spaceman limits get --space <space>`.
- Write down: SpaceMan version, node agent version, Claude Code version and the date. Label the accounts A and B; record
  no email or account identity (K-17).

## Procedure

Record the observed result next to each step. A step that doesn't behave as described is a finding: stop and note it, don't
work around it.

### 1. Baseline

- [ ] Launch a worker (any role) in the space. It starts on account A and works (a short coding task is enough).
- [ ] `spaceman account space-windows --space <space> -o json` (the table shows only account, name and harness; the used
      percentage and reset time are in the JSON): account A shows a 5-hour window with a used percentage after the
      run's first turn. Compare it with what Claude Code's own `/usage` shows in the run's terminal. Note the difference.
- [ ] `spaceman usage space --space <space> --group-by account`: tokens appear for account A only.

### 2. Reach the limit on A

- [ ] Keep giving the worker work until account A's 5-hour limit is reached (or use account A while it is already close to
      its limit). Real limits can't be forced by SpaceMan; this is the slow step.
- [ ] Note the time the limit appeared and the reset time Claude Code printed.
- [ ] The reset must be **more than 15 minutes away**, otherwise the run waits instead (that is the near-reset case, step 5).

### 3. The switch

- [ ] SpaceMan arms a switch, waits for the run's idle point, stops the run and starts a new run of the same agent on
      account B. Check with the runs list and the audit log
      (`spaceman audit list --target <old run ID>`; the actions are `account.switch_started`, `_parked`, `_resumed` and
      `_succeeded`).
- [ ] The new run's session ID equals the old run's, and it continued the conversation (ask it what it was doing: it must
      know). Its credential is B's: both `/status` in its terminal and the `oauthAccount` in
      the run's `.claude.json` must name account B, never A (compare locally; don't write the identity down).
- [ ] The task follows the new run.
- [ ] Control got **one** message about the move and none saying "parked" or "resumed".
- [ ] The new run finishes a turn: the switch is recorded as succeeded.

### 4. Usage

- [ ] `spaceman usage space --space <space> --group-by account`: both accounts have tokens, B's only for after the move.
      The sum is about what Claude Code reports for the whole session (the session is counted once, not twice).
- [ ] `spaceman usage space --space <space> --group-by role`: the role's total equals the sum of both accounts.
- [ ] `spaceman usage account <B> --group-by role` shows the role.
- [ ] `spaceman account windows <A> -o json` still shows the 5-hour window near 100 % with its reset time (JSON output
      shows used % and reset; the table doesn't); `<B>` shows its own. Note any window that is missing, stale or wrong.

### 5. A near reset waits (optional, needs a second limit event)

- [ ] When an account's limit resets in under 15 minutes, a run that reaches it is **not** moved: it stays alive on the same
      account and carries on after the reset (control gets one "at its usage limit" message and one "cleared" message).

### 6. Headroom gate

- [ ] `spaceman limits set --space <space> --set usage_limit_min_headroom_percent=20`. With account A's window above 80 % used, launch a new worker with no
      account named: it starts on B. A worker already running on A is not touched.
- [ ] With both accounts' windows above 80 % used, a launch is refused with a wait that ends at the earliest reset
      **or one hour from now, whichever is sooner**. The refusal creates no hold row, so `spaceman limits holds` won't
      show it. (A window not observed in the last hour counts as unknown and is not refused.)
- [ ] Set it back to 0: `spaceman limits set --space <space> --set usage_limit_min_headroom_percent=0`.

### 7. Pool exhausted

- [ ] With A at its limit and B limited as well (or after B reaches its limit), a worker that hits the limit is parked, not
      moved. Control gets one message saying every account of the pool is limited, and the space gets **one** human action
      for the window, however many runs hit it. This notice is separate from the "reset more than 12 hours away" one and
      has no such threshold. The run resumes at the earliest reset.
- [ ] Remove the human action when done.

## What to record

Fill this in, and copy the summary into the Real evidence column of SM-RUN-007, SM-OBS-007 and SM-ACCT-007 in
[TRACEABILITY.md](TRACEABILITY.md), saying which steps passed. Don't upgrade a status the checklist didn't cover.

| Item | Value |
|---|---|
| Date run | |
| SpaceMan / node agent / Claude Code versions | |
| Steps passed | |
| Steps failed or skipped, and why | |
| Same session ID after the switch | yes / no |
| Windows agreed with Claude Code's own display | yes / no / how far off |
| Findings (open an issue for each) | |

## Cleanup

- [ ] Stop the test runs and archive or delete the space.
- [ ] Unbind and delete the two accounts if they were created only for this, and revoke the setup tokens.
- [ ] Make sure no token, transcript or screenshot with account details was kept.
