# Limits and budgets

Every limit SpaceMan enforces is part of one model. The installation sets a default for each limit in its
configuration file, a space may override any of them, and the space's **Limits** section shows the value in force, where
it comes from and where SpaceMan enforces it.

<Shot name="space-limits" alt="A space's Limits page: the budget card with its usage-reporting note, and the effective limits with their sources" caption="A space's effective limits" />

## What you can limit

| Limit | Default | Bounds | Bounds what | Enforced at |
|---|---|---|---|---|
| Concurrent runs (`max_concurrent_runs`) | 8 | 1–256 | worker and diagnostic runs active in the space | run launch |
| Running top-level tasks (`max_top_level_tasks`) | 1 | 1–20 | top-level queue tasks running at once | queue selection; set per space in the [queue policy](/guide/queue#policy) |
| KB support sessions (`max_kb_sessions`) | 8 | 1–64 | active support sessions across the space's knowledge bases | KB scheduler |
| Support depth (`support_depth`) | 1 | 1–3 | how deep support may nest | KB ask tool |
| Pending human actions (`max_human_pending`) | 500 | 1–5000 | open human actions in the space | human-action creation |
| Pending human actions per run (`max_human_pending_per_run`) | 25 | 1–500 | open human actions one agent run asked | human-action creation |
| Tool calls per run (`max_tool_calls_per_run`) | 5000 | 0–1,000,000 (0: none) | SpaceMan and MCP gateway tool calls of one run | agent tool gateway |
| Event ingestion rate (`ingest_per_minute`) | 600 | 1–60,000 | events one subscription ingests per minute | event ingestion |
| Event automation depth (`event_depth`) | 8 | 1–64 | SpaceMan ingestions an inbound event may already have passed through; deeper events are quarantined ([event loops](/guide/events#event-loops-between-installations)) | event ingestion |
| Task deadline (`task_deadline_minutes`) | none | 0–43,200 | how long a top-level task runs before you are told | deadline timer |
| Attachment storage (`storage_quota_bytes`; installation default `artifacts.space_quota_bytes`) | 1 GiB | 1 MiB–1 TiB | attachment bytes stored for the space | uploads and event payloads |
| Token budget, 24 h (`budget_tokens_24h`) | none | 0: none | reported tokens of runs started in the last 24 hours | run launch |
| Cost budget, 24 h (`budget_cost_cents_24h`) | none | 0: none | harness cost estimates, in US cents | run launch |
| Token budget per run (`budget_tokens_per_run`) | none | 0: none | reported tokens of one run | warning only |
| Budget warning (`budget_warn_percent`) | 80 | 1–100 | when budget warnings are sent | usage reports |
| Terminal recording size (`recording_quota_bytes`) | 512 MiB | 1 MiB–64 GiB | one run's recording on the node | the node's recorder, from the launch |
| Export size (`export_max_bytes`) | 2 GiB | 1 MiB–64 GiB | one space export archive | space export |
| Recording, transcript, attachment and finished human-action retention (`retention_*_days`) | kept (0) | 0–36,500 days | how long central keeps them | the daily retention pass ([Retention and export](/guide/retention)) |
| Start control on new work (`control_autostart`) | on (1) | 0 off, 1 on | whether work waiting for control starts the space's control role ([Control](/guide/control#starting-and-stopping-on-its-own)) | control supervisor |
| Stop idle control (`control_idle_stop_minutes`) | 30 | 0–10,080 (0: never) | how long control sits idle before it is stopped | control supervisor |
| Usage-limit failover (`usage_limit_failover`) | off (0) | 0 off, 1 on | when the account a launch would use is held at a usage limit, use another account bound to the space for the same harness that isn't held; a space routed through Harbor Pilot never falls back to a direct account | run launch |
| Usage-limit keep-alive (`usage_limit_keep_alive_max_minutes`) | 360 | 1–1,440 | how long a run at a usage limit waits for the reset with its process kept alive (after that, or at once when the reset is further away, SpaceMan stops it and parks it to resume at the reset); also the longest gap between re-checks of a hold whose reset time is unknown | usage-limit holds and parking |
| Usage-limit maximum wait (`usage_limit_max_wait_hours`) | 168 (7 days) | 1–720 | how long a worker or diagnostic run parked at a usage limit waits to be resumed before its task goes back to the queue, ready and unassigned; control is never given up on | usage-limit waits |
| Usage-limit people notice (`usage_limit_notify_after_hours`) | 12 | 0–720 (0: never) | how far away the reset of an account held at a usage limit must be for the space's people to get one human action about it, once per account and limit window (12 h: weekly limits, not 5-hour ones) | usage-limit holds |

Some limits are set elsewhere and only shown here: the node's **capacity** (the node agent's `capacity` setting, shared
by every space on the node), each knowledge base's own session limit, human-action rate limits per type
([policies](/guide/human-actions)), the MCP gateway's limits per server, dream budgets and the installation-wide audit
retention (`retention.audit_days`).

Retention periods are the one kind of limit whose lowering removes something: the next daily pass deletes what is older
than the new period. Changing them needs `retention:write` (admins); see [Retention and export](/guide/retention).

A launch beyond a limit is refused with `rate_limited` and a message that names the limit; control gets the same answer
from its tools.

## Separate pools: no starvation, no recursion

Workers and diagnostic runs share the space's concurrency limit and the node's capacity. Control (one per space), KB
support sessions and dreamer runs have their own pools and never count against them, so busy workers cannot keep a
knowledge base from answering, and many support sessions cannot keep workers from starting.

A worker or diagnostic run that is waiting for its Claude usage limit to reset (its process kept alive until the reset)
doesn't count against the space's concurrency limit, so another run can start meanwhile. It still counts against the
node's capacity, because its process is still running there.

### Parked runs

A run isn't kept alive for longer than the **usage-limit keep-alive** (6 hours by default). When its reset is further
away than that (a weekly limit), SpaceMan stops it at once; when the limit is still open after the keep-alive, it stops
it then. A run that ends at its limit, or whose node is lost while it waits, is treated the same way. The run is
**parked**: its process ends, but its session and its task are kept, and a **wait** brings it back.

- **At the reset** (2 minutes after it, or at the hold's next re-check when the reset time is unknown), SpaceMan resumes
  the run as a new run of the **same instance**, resuming its conversation (`--resume`), with a short prompt saying the
  limit reset. Its task now points at the new run. Control comes back first, then the oldest waits, a few at a time,
  so a reset doesn't start every waiting run at once.
- **If it can't resume yet**, the wait keeps waiting: the space is paused, its node isn't connected, the space is at its
  run limit, or the account is still held. A resume never stops another run to make room. When the node connects again
  or the space resumes, waiting runs resume at once.
- **If the space moved to another node** since, the conversation can't be carried over (sessions stay on their node):
  the run starts a fresh session there with its original prompt and a note, and control is told.
- **After the maximum wait** (7 days by default, `usage_limit_max_wait_hours`), SpaceMan gives up on a worker or
  diagnostic run: its task goes back to the queue, ready and unassigned, and control is told. Control itself is never
  given up on.
- A wait ends without a resume when its task is finished, cancelled, back in the queue or given to another run, or when
  the space is archived.

**Runs through Harbor Pilot.** A gateway run's limit names no reset: Harbor Pilot refuses with "no eligible account"
when every account it could route to is exhausted, or names the blocking budget. SpaceMan then estimates the reset
from Harbor Pilot itself:

- **Exhausted accounts:** the earliest reset among the accounts Harbor Pilot's quota board shows as unavailable. The
  board lists every account of the Harbor Pilot installation, not only the ones the space's plan uses, so this is an
  estimate. A run may resume early, hit the limit again and be parked again.
- **A used-up budget:** the budget's period end.

SpaceMan reads Harbor Pilot again every 15 minutes, and at once when a space subscribed with the Harbor Pilot quota
event template receives `quota.window.recovered`. When the account it waited for is available again, or the budget no
longer blocks, the hold clears and parked runs resume. If that account's reset moves later instead, the estimate moves
with it. Launches refused meanwhile say "until about …" with the
estimate. It needs the connection's service-principal token (`usage.read`). Without it, or while Harbor Pilot names no
reset, the hold is re-checked by letting one launch through from time to time, as for any unknown reset.

KB support and dreamer runs are neither stopped at the keep-alive nor parked: their schedulers start them when needed.

### Telling people about long waits

When an account (or the Harbor Pilot gateway) is held until a reset more than **12 hours** away
(`usage_limit_notify_after_hours`), typically a weekly limit, the space gets one **human action**: an informational
message from SpaceMan ("Usage limits") with normal priority, in the space's Needs-you list. It names the account, the
reset time, and how many runs (and tasks) are waiting on it, and says what you can do: nothing, if the work can wait;
bind another account of the same harness and turn on usage-limit failover; or, if the account works again, clear the
hold. The action gives the hold's ID: `spaceman limits clear <hold id> --space <space>` clears it in this space only
(`spaceman limits holds --space <space>` lists the space's holds), and launches on the account stay refused while
another space still holds it; `spaceman account clear-holds <account>` (needs `account:write`) clears the account's
holds in every space.

There is one such action per space and limit window, however many runs hit the limit and however often they report it.
A 5-hour wait raises none. A hold whose reset time is unknown raises none either, since nobody knows the wait is long.
Set the limit to 0 to turn the notice off for a space. Control is told about runs at their limit separately (above).

`spaceman limits waits` lists a space's waits. `spaceman limits resume <wait>` resumes one now (the usual launch checks
apply), and `spaceman limits release <wait>` gives one up now; both need the permission that launches runs in the space
and are audited.

Support never starts more support without bound. At the default **support depth** of 1, a KB support session cannot ask
any knowledge base; raising the depth allows that many levels. KB sessions never launch workers.

## Lowering a limit never stops work

Lowering a limit only refuses new work beyond it. Runs keep running, open human actions stay open, stored files stay,
and a run over its tool-call limit keeps running without more tool calls.

The one exception is a choice you make per space: **When the concurrent-runs limit is lowered → Stop the newest runs** stops the newest worker and
diagnostic runs beyond the new limit (interrupt, then kill after the grace), so the oldest keep running. The default,
**drain**, lets them finish. A passed task deadline is reported to control and to the operators once; the task is never
stopped, failed or moved.

## Budgets are usage-based

::: warning Not a hard currency limit
Budgets use the usage the harnesses report. That usage arrives late, often only when a run ends, or not at all (Codex
reports none). SpaceMan therefore never claims a hard currency limit: a budget warns and stops **new** runs once the
reported usage passes it. Runs in progress are never stopped, and runs with unknown usage are shown as unknown, not
counted as zero.
:::

- The window is the last 24 hours, rolling, over runs started in it.
- At the warning share (80 % by default) the operators get one message per budget and window in the space's
  **Messages**; past the budget they get another, and new runs of every kind are refused until reported usage falls
  below it or you raise the budget.
- The cost budget adds up the harnesses' own estimates, with their assumptions (see
  [Usage and transcripts](/guide/usage)).

**Hard limits come from Harbor Pilot.** A space that routes runs through the [Harbor Pilot
gateway](/operators/harbor-pilot) also shows a **Harbor Pilot budgets** card on this page. Those budgets are enforced by
Harbor Pilot itself on every request, and a used-up one refuses new launches of the routed harnesses at once, naming
the budget, its spend and when it resets ([Budgets and usage](/operators/harbor-pilot#budgets-and-usage)). Usage that
Harbor Pilot reads back for a Codex run counts toward the budgets above like any reported usage.

## Configuration and API

Installation defaults are the `limits:` section of the configuration file (and `artifacts.space_quota_bytes` for
storage); values outside the bounds above are refused at start.

```yaml
limits:
  max_concurrent_runs: 8
  max_kb_sessions: 8
  support_depth: 1
  task_deadline_minutes: 0      # no deadline
  budget_tokens_24h: 0          # no budget
  budget_warn_percent: 80
  control_autostart: 1          # start control when work waits for it (0: off)
  control_idle_stop_minutes: 30 # stop control after 30 idle minutes (0: never)
```

- `GET /api/v1/spaces/{space_id}/limits`: every limit with its value, default, override and source, the related limits
  and the budget state.
- `PUT /api/v1/spaces/{space_id}/limits` with `{"values": {...}, "on_lower": "drain"}` replaces the space's overrides
  (keys left out inherit). Send `If-Match` once the space has stored overrides. Changes are audited with the old and
  new value of each limit.
- `GET /api/v1/limits/definitions` lists every limit with its unit, bounds and enforcement point.

Viewers of a space can read its limits; changing them needs the space configuration permission (operators and admins).
