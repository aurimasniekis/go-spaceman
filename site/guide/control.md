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
- a task moved to review, blocked or failed by someone else;
- a worker run that ended, with its task's state.

When control's program is idle it is told that messages are waiting. A control run started later finds them too.

## Recovery

If a control run ends while it was still wanted (it crashed or its harness exited), SpaceMan starts a new control run
on the same instance, **resuming its session** when the harness supports it. Recovery is bounded: after three restarts
within an hour, SpaceMan stops and raises an urgent [human action](./human-actions), "Control keeps stopping", with the
steps to check. A control run that an operator stopped, or one in a paused space, is never restarted.
