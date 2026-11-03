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
new incident preempts running ordinary tasks, and whether escalation may reorder.

## From the inbox

An inbox item is not a task. **Create task** on an item turns it into one: the task keeps a link to the item, and the
item is marked done and linked to the task.
