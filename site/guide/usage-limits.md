# When an account runs out

A claude.ai subscription has a 5-hour window and a weekly window. When a Claude Code run uses one up, Claude Code ends
the turn and refuses new requests until the window resets. A Harbor Pilot gateway does the same when every account it
could route to is exhausted, or when a budget blocks. SpaceMan notices this and does four things:

- It **holds the account** until the reset, so no new run is started into the same limit.
- It keeps a short wait **alive** and **parks** a long one, keeping the run's session and task.
- At the reset it **resumes** parked runs with their conversation.
- It **tells control and the people** of the space, once each.

The design is recorded as ADR-0079 in the [decision log](/security/decisions). Before you rely on it, read
[what isn't covered](#what-isn-t-covered): no real usage limit has been observed on a SpaceMan run yet.

## What SpaceMan notices

The node agent watches each Claude Code run. When the run goes idle, reports an error or ends, the agent reads the new
records of the run's transcript. It reads only typed fields, never the screen and never message text, so a change in
Claude Code's wording can't mislead it.

- **A subscription limit** comes as a record that names the limit and, usually, its reset time and kind: the 5-hour
  window, the weekly window, the weekly Opus or Sonnet window, or overage. In more than half of the waits examined,
  Claude Code took another path that names neither a reset time nor a kind. SpaceMan then knows only that the run is at
  "a usage limit".
- **A gateway limit** comes as Harbor Pilot's refusal: "no eligible account is available" when the accounts are
  exhausted, or the name of a used-up budget. Neither names a reset time.
- **The limit clears** when Claude Code records the reset or continues by itself, or when a later reply uses tokens.

Only Claude Code runs are watched, and only by a node agent of this release or later (protocol minor 12). An older node
agent reports nothing, and its runs behave as before. Nothing from the transcript other than the kind and the times
reaches central or its logs.

## The account is held

A run at its limit opens a **hold** on the account it used (or on the Harbor Pilot connection, or on the project of a
used-up budget). While the hold is open:

- **Launches on that account are refused**, in every space bound to it, with `rate_limited` and the time the hold lets
  launches through again, in UTC. A weekly Opus or Sonnet limit holds the account for every model.
- **Control isn't restarted into the limit.** If control can't start because of a hold, starting it on new work waits
  until the reset.
- **Other spaces see a shared hold.** A space whose account another space found exhausted lists the hold as *shared*,
  without the other space's details.

The hold ends at the reset, when any run on the account makes progress again, when a node reports the limit cleared, or
when someone clears it. When the reset time is unknown, SpaceMan lets **one** launch through from time to time to see
whether the account works again: after an hour, then two, four and so on, at most every **usage-limit keep-alive**
(6 hours by default). A launch that hits the limit again moves the next try on; one that gets a reply clears the hold.

**Failover** is off by default. Turn on `usage_limit_failover` for a space, and a launch whose account is held uses
another account bound to the space for the same harness that isn't held. A space routed through Harbor Pilot never
falls back to a direct account.

## Short waits: kept alive

When the reset is at most the **usage-limit keep-alive** away (`usage_limit_keep_alive_max_minutes`, 6 hours by
default), or unknown, SpaceMan leaves the run's process alone, and Claude Code usually continues by itself after the
reset (a gateway run is different; see [below](#runs-through-harbor-pilot)). Meanwhile the run:

- doesn't count against the space's concurrency limit, so another run can start, but still counts against the node's
  capacity, because its process is still running there;
- gets no notices: messages to it wait until the limit clears;
- if it is control, isn't stopped as idle.

## Long waits: parked

A run isn't kept alive for longer than the keep-alive. When its reset is further away than that (a weekly limit),
SpaceMan stops it at once; when the limit is still open after the keep-alive, it stops it then. A run that ends at its
limit, or whose node is lost while it waits, is treated the same way. The run is **parked**: its process ends, but its
session and its task are kept, and a **wait** brings it back.

- **At the reset** (2 minutes after it, or at the hold's next try when the reset time is unknown), SpaceMan resumes the
  run as a new run of the **same instance**, resuming its conversation (`--resume`), with a short prompt saying the
  limit reset. Its task now points at the new run. Control comes back first, then the oldest waits, a few at a time,
  so a reset doesn't start every waiting run at once.
- **If it can't resume yet**, the wait keeps waiting: the space is paused, its node isn't connected, the space is at its
  run limit, or the account is still held. A resume never stops another run to make room. When the node connects again
  or the space resumes, waiting runs resume at once.
- **If the resume fails** (the run ends before its first turn), SpaceMan tries once more in a fresh session with the
  run's original prompt, and control is told.
- **If the space moved to another node** since, the conversation can't be carried over (sessions stay on their node):
  the run starts a fresh session there with its original prompt and a note, and control is told.
- **After the maximum wait** (`usage_limit_max_wait_hours`, 7 days by default), SpaceMan gives up on a worker or
  diagnostic run: its task goes back to the queue, ready and unassigned (a task in review stays in review), and control
  is told. Control itself is never given up on.
- A wait ends without a resume when its task is finished, cancelled, back in the queue or given to another run, or when
  the space is archived.

KB support and dreamer runs are neither stopped at the keep-alive nor parked: their schedulers start them when needed.

## Runs through Harbor Pilot

A gateway run's limit names no reset, so SpaceMan **estimates** one from Harbor Pilot itself:

- **Exhausted accounts:** the earliest reset among the accounts Harbor Pilot's quota board shows as unavailable. The
  board lists every account of the Harbor Pilot installation, not only the ones the space's plan uses, so this is an
  estimate. A run may resume early, hit the limit again and be parked again, or resume up to 15 minutes late.
- **A used-up budget:** the budget's period end.

SpaceMan reads Harbor Pilot again every 15 minutes, and at once when a space subscribed with the Harbor Pilot quota
event template receives `quota.window.recovered`. When the account it waited for is available again, or the budget no
longer blocks, the hold clears and parked runs resume. If that account's reset moves later instead, the estimate moves
with it. Launches refused meanwhile say "until about …" with the estimate.

This needs the connection's service-principal token (`usage.read`; see
[Harbor Pilot gateway](/operators/harbor-pilot)). Without it, or while Harbor Pilot names no reset, the hold is tried
from time to time like any hold with an unknown reset. A gateway run is kept alive for at most 30 minutes before it is
parked, because Claude Code doesn't wait for a gateway by itself.

## Who is told

**Control** gets one message when one of its runs is kept alive at its limit and one when the limit clears; one when a
run is parked, saying when it resumes and when its task would be given back; and one when it resumes or is given up.
Don't relaunch or reassign a parked run's task: its wait brings it back. See
[Control and workers](./control).

**People** are told only about long waits. When an account (or the Harbor Pilot gateway) is held until a reset more
than **12 hours** away (`usage_limit_notify_after_hours`), typically a weekly limit, the space gets one **human action**:
an informational message from SpaceMan ("Usage limits") with normal priority, in the space's Needs-you list. It names
the account, the reset time, and how many runs (and tasks) are waiting on it, and says what you can do (below).

There is one such action per space and limit window, however many runs hit the limit and however often they report it.
A 5-hour wait raises none. A hold whose reset time is unknown raises none either, since nobody knows the wait is long.
Set the limit to 0 to turn the notice off for a space.

**Subscribers** of SpaceMan's own events get `fleet.usage_limit.reached` and `fleet.usage_limit.cleared` once per hold
([Publishing SpaceMan events](./publishing-events)).

## What you can do

Usually nothing: if the work can wait, the waits bring it back. Otherwise:

- **Look.** A run at its limit shows the limit and its reset time ("at usage limit"), and a parked run says so. The
  space's **Limits** page has a **Usage limits** card: the held accounts with until when, and the waiting runs with
  when they resume and when they would be given up. Times are in your time zone, with UTC in the tooltip.
- **Resume a run now** (**Resume now**, `spaceman limits resume <wait>`). The usual launch checks apply, so a resume on
  a held account is still refused.
- **Give a run up now** (**Release**, `spaceman limits release <wait>`). A worker's task goes back to the queue.
- **Clear a hold** when you know the account works again (**Clear**, `spaceman limits clear <hold> --space <space>`).
  This clears it in your space only: launches on the account stay refused while another space still holds it. With
  `account:write`, **Clear in every space** (`spaceman account clear-holds <account>`) clears the account's holds in
  all spaces.
- **Add capacity:** bind another account of the same harness to the space and turn on usage-limit failover.

Resuming, releasing and clearing in a space need the permission that launches runs there (operators and admins), and
each is audited. `spaceman limits holds` and `spaceman limits waits` list a space's holds and waits.

## Settings

| Limit | Default | Bounds | What it does |
|---|---|---|---|
| `usage_limit_keep_alive_max_minutes` | 360 | 1–1,440 | longest wait kept alive; also the longest gap between tries of a hold with an unknown reset |
| `usage_limit_max_wait_hours` | 168 (7 days) | 1–720 | how long a parked worker or diagnostic run waits before its task goes back to the queue |
| `usage_limit_notify_after_hours` | 12 | 0–720 (0: never) | how far away a reset must be for the people notice |
| `usage_limit_failover` | off (0) | 0 off, 1 on | use another unheld account of the same harness |

They are space limits like any other: installation defaults in the `limits:` section of the configuration file,
overrides per space ([Limits and budgets](./limits)).

## What isn't covered

- **No real usage limit has been observed on a SpaceMan run.** A real subscription limit can't be forced in a test.
  Every test of this feature is simulated: scripted node reports, a fake harness, a fake clock and a fake Harbor Pilot.
  The gateway side was also checked against a real Harbor Pilot demo installation, with a scripted node. Resuming a
  real parked session, and resuming it under another real account (failover), are not verified.
- **Codex isn't covered.** Codex runs are never detected at their limit, so they are neither kept alive nor parked, and
  they open no hold. A Codex launch through a Harbor Pilot connection that a Claude Code run found exhausted is still
  refused while the hold is open.
- **A gateway reset is an estimate**, from the whole Harbor Pilot installation's quota board.
- **Claude Code's transcript format is undocumented.** If it changes, detection stops and runs behave as they did before
  this feature: nothing is held or parked.
- **A run's own transcript can hold its account.** A forged record can block an account for at most 8 days; operators
  can clear the hold, and any progress on the account clears it.

The full list is in [Known limitations](/reference/limitations#usage-limits).
