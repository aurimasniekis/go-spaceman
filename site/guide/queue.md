# The execution queue

Each space has one **execution queue**: the accepted work, in a single explicit order. Control (the space's coordinating
agent) and operators pick work from the top; SpaceMan enforces the mechanics and records why tasks were skipped.

<Shot name="queue" alt="Queue with a running task, a ready high-priority task, an incident moved down, a blocked task, a dependent task and a backlog item" caption="A space's queue" />

## Order

- A new task is inserted by **priority**: after the active tasks with the same or a higher priority. Incident tasks go to
  the top when the [queue policy](#policy) says so.
- After that, **operators decide**. Drag a row, or focus it and press **Alt+↑ / Alt+↓**, or use the arrows. A moved task
  is marked as placed by an operator (the hand icon): the policy never moves it again, and a repeated trigger, such as
  the same alert firing again, never brings it back to the top.
- **Escalate** raises a task to urgent. It moves the task to the top only when the policy allows escalation to reorder.
- Moving a task never stops work that is running.

## Eligibility and selection

The queue shows the **next eligible** task: the highest ready task that can start now. Ready tasks that cannot start say
why ("waiting"):

- a **dependency** is not done yet (dependency cycles are rejected);
- an open **blocker**: a human action it waits for, or anything an operator or control recorded;
- an **exclusive resource** (such as `env:staging`) is held by a running task;
- the **active limit**: by default one top-level task runs at a time, with any number of subtasks (worker steps) under
  it;
- for a subtask, its parent is not running.

A blocked task never blocks the queue: the next eligible task is selected instead, and the decision and its reasons can
be recorded.

<Shot name="queue-task" alt="Task drawer of a ready task waiting for its dependency, with state, dependencies and blockers" caption="A task waiting for its dependency" />

## States

Tasks move through **backlog, ready, running, blocked, review, done, failed** and **cancelled**. Starting a task requires
it to be eligible. Returning a running task to ready is a **preemption** and is counted, not treated as a failure.
Nothing is accepted automatically: a worker finishing or a person answering a question never marks a task done; control
or an operator decides.

## Reassigning a task

The task drawer's **Assignee** form sets the role a task is for and, optionally, a run of the space that should work on
it (choosing a run fills in its role); clear either to unassign it. The change is sent with the task's version, so a
concurrent edit is refused, and the audit log records the previous and the new assignee. Reassigning never stops or
starts a run: control acts on the new assignee. A run of another space is refused. From the command line:
`spaceman task update --space payments TASK --role reviewer --run RUN_ID` (`--run ""` clears the run).

<Shot name="queue-reassign" alt="A task's drawer with its Assignee form: a role and a run of the space chosen, and a Reassign button" caption="Reassigning a task to a role and a run" />

## Attachments

A task's objective is written in the same Markdown composer as inbox items: paste, drop or attach images and files.
They are stored as the space's attachments, listed in the task's drawer, and given to agents as
`spaceman://attachment/<id>` references. A task created from an inbox item also references the item's attachments.

## Policy

**Policy** sets how many top-level tasks may run at once, where new incident tasks go (top or by priority), whether a
new incident preempts running ordinary tasks, whether escalation may reorder, and the incident rules below.

## Incidents

An incident is a task of kind `incident`. The queue policy decides who may raise one and what happens to the work it
interrupts.

- **Trusted triggers.** By default operators (the UI, the API, the CLI) and control may raise incidents. The policy can
  also trust an event subscription (`subscription:<id>`) or an event template (`template:<id or slug>`): when control
  creates an incident from an inbox item, the trigger is the item's source, not control. Workers, schedules, other
  spaces and other inbox sources can never be trusted. Remove `control` to make control relay only trusted sources.
- **Untrusted requests** are **proposed** (default) or **refused**. A proposal is added as an ordinary task, by
  priority, without preempting anything, and shows a *Proposed incident* badge; an operator confirms it (it becomes an
  incident raised by that operator, and insertion and preemption apply then) or dismisses it.
- **Preemption** returns running ordinary top-level tasks to ready. Their workers (the runs assigned to the task and
  its subtasks) **finish** (default: they keep going and are told to wrap up; their review report is still accepted),
  are **paused** (their current turn is interrupted; the run stays, idle, with its session) or are **interrupted**
  (stopped, with a resumable session). Every preempted task keeps a preemption log in its drawer, and control is told.
- **Incident capacity** gives incidents extra top-level slots, so an incident can run beside ordinary work without
  preempting it.
<Shot name="queue-policy" alt="Queue policy dialog with incident triggers, untrusted-source handling, worker handling, incident capacity and resumption" caption="The incident fields of the queue policy" />

- **Resumption.** When the incident is done, failed or cancelled, **manual** resumption (default) tells control which
  tasks wait; **auto** resumes them in queue order: an eligible task with a live worker runs again and paused workers
  are told to continue; a task whose workers were stopped stays ready for control to launch a new one. **Resume** in
  the task drawer (or `spaceman task resume`, or control's `spaceman_task_resume`) resumes one task by hand.

<Shot name="queue-preempted" alt="Drawer of a preempted task with a Resume button and its preemption log" caption="A task an incident preempted" />

From the command line:

```sh
spaceman task set-policy --space payments --incident-preempts --incident-trigger operator \
  --incident-trigger template:pagerduty-alert --incident-workers pause --incident-resume auto
spaceman task incident --space payments TASK confirm   # or dismiss
spaceman task resume --space payments TASK
```

Every decision is audited: `task.incident_proposed`, `task.incident_refused`, `task.incident_decided`,
`task.preempted` (with each worker and what was done to it) and `task.resumed`.

## From the inbox

An inbox item is not a task. **Create task** on an item turns it into one: the task keeps a link to the item, and the
item is marked done and linked to the task.
