# Control and workers

Each space has at most one **control** run: the agent that coordinates the space. Control reads the inbox, manages
the queue, starts and stops **workers** for tasks, and decides when work is done. People stay in charge: operators can
pause the space, reorder or cancel work, stop control, and launch it again.

## What control can do

Control has the tools every run has (see [Agent tools](./agent-tools)) plus the orchestration tools:

| Tool | What it does |
|---|---|
| `spaceman_worker_launch` | Start a worker for a **ready, eligible** task. The task becomes running and is assigned to the worker. |
| `spaceman_worker_stop` | Stop a worker (or a KB, dreamer or diagnostic run). Interrupt keeps the session resumable. |
| `spaceman_runs_list` | The space's active runs with their role, program status and task (every run may read it). |

Workers cannot launch runs at all, and no tool starts a control run, so a worker can never promote itself. Control
cannot stop itself or another control run; operators do that.

A worker starts with the task as its first instruction (title, objective, acceptance criteria and anything control
adds), then reports with `spaceman_task_set_status` (**review**, or **blocked**). Nothing is accepted
automatically: control or an operator moves the task to done.

## Separate worktrees for writers

When control starts a worker with a repository (`repo`), the worker gets its **own git worktree** of that repository
in the space folder, on its own branch (by default `spaceman/<task id>`, created from the repository's HEAD). Two
writers never share a working tree. Operators can do the same from **Launch run** with **Git worktree of**. `repo` is a
[configured repository](./repositories) (its name or folder) or another folder in the space; a configured repository
that is not cloned yet is cloned first when its clone policy allows it.

SpaceMan never resets, deletes, commits, pushes or merges anything:

- an existing worktree for the branch is reused as it is, with its uncommitted work;
- a folder that is in the way is left alone, and the launch fails with the reason;
- git runs with the node's own configuration and credentials; SpaceMan does not manage git credentials.

Agents open pull requests with the tools they have; the branching policy is the team's.

## Keeping control informed

Control does not poll. SpaceMan sends it a **notification** (a durable [message](./messages)) when something needs its
attention:

- a new inbox item it did not add itself;
- a new task someone else added to the queue (an operator in the UI, the API or the CLI);
- a task made ready (it can start), or moved to review, blocked or failed, by someone else;
- a worker run that ended, with its task's state;
- an incident that preempted running work (which tasks, and what happened to their workers), a source that only
  proposed an incident, and an incident that ended with the preempted tasks that wait or were resumed (see
  [Incidents](./queue#incidents)).

When control's program is idle it is told that messages are waiting. A control run started later is told about them
too, including ones an earlier control was told about but never acknowledged.

## Starting and stopping on its own

You do not have to keep control running. When work waits for control and no control run is live, SpaceMan **starts
control** itself. It does not need a new event: anything waiting for control that no control has seen yet counts, such
as a new inbox item, a task to decide on, a worker that ended, a message from an agent, or a person's response to a
[human action](./human-actions) whose requester has ended (for example control's own question, answered after control
was stopped). The control that starts, or one an operator launches later, is told once about each response it has
not acknowledged yet.

- It **resumes the last control run**: the same role, harness, instance and account, and the same session when the
  last run finished at least one turn. A space that never had a control run gets its control role (the space role
  named `control`, or the first role whose template is of kind control).
- It does not start control while the space is paused, archived or relocating, or while its node is away. The work
  waits until then.
- When the launch is refused, for example because no account fits or a budget is spent, SpaceMan raises a human action,
  "Control could not start", once an hour with the reason. It also stops after six starts in an hour, so a control
  that keeps ending cannot loop.

A control run that has been **idle** (its turn finished) for 30 minutes is **stopped**. SpaceMan does not count this as
a failure and does not restart it. The next work starts control again, resuming its session. Control stays running
while it works, while an operator has [taken over its terminal](/operators/terminals#taking-over-a-terminal), and while something waits for it that it has
not been told about yet.

Both behaviours are [limits](./limits) of the space. Turn **Start control on new work** (`control_autostart`) off to
start control only by hand. Set **Stop idle control** (`control_idle_stop_minutes`) to 0 to keep an idle control
running. Turning auto-start off never stops a control that is running.

## Recovery

If a control run ends while it was still wanted (it crashed or its harness exited), SpaceMan starts a new control run
on the same instance, **resuming its session** when the harness supports it. Recovery is bounded: after three restarts
within an hour, SpaceMan stops and raises an urgent [human action](./human-actions), "Control keeps stopping", with the
steps to check. A control run that an operator stopped, or one in a paused space, is never restarted.

An operator **restarts control** by stopping its run (Runs, or `spaceman run stop`) and launching control again:
SpaceMan never restarts a run an operator stopped, and a second control run is refused while one is wanted. With
auto-start on, the next waiting work starts a stopped control again. Turn auto-start off to keep it stopped.

A restart resumes the session only when the run finished at least one turn. A session with no turn was never saved,
so it cannot be resumed.

## Diagnostics

A **diagnostic** run (kind `diagnostic`, or a space role of that kind) investigates a space without being a worker. Launching
one needs `run:diagnose` on top of `run:control`; operators and admins hold both, viewers neither. The launch is
audited with its kind like every launch. Control cannot launch diagnostics: its tool starts workers only.
