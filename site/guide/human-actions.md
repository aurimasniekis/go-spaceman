# Human actions

A **human action** is a request for a person: a question, a permission approval, a to-do, or a message. Human actions
are kept apart from the [inbox](./inbox) (incoming material), the [execution queue](./queue) (accepted work) and agent
messaging, so a person always sees one list of what waits for them.

Each space has its own list under **Human actions**. **Needs you** in the main navigation shows every pending action
across the spaces you can read, oldest first, and its badge counts them. Spaces you cannot read are never shown or
counted.

<Shot name="human-global" alt="Needs you page listing a blocking question, an approval, two to-dos and an informational message across two spaces" caption="Everything waiting for a person, across spaces" />

## Types

| Type | Asks for | Response |
|---|---|---|
| **Question** | An answer: one of the offered options, a written answer, or both | **Send answer**: outcome *answered* |
| **Approval** | Permission to do something | **Approve** or **Deny** (a denial says why) |
| **To-do** | Something a person does outside SpaceMan, optionally as a checklist | **Mark completed** once every step is ticked |
| **Message** | Attention. It is informational (*FYI*) unless it needs an acknowledgement | **Acknowledge** |

Each type has its own action, so a person never "approves" a question or "acknowledges" an approval. Comments are
discussion: they never count as a response, and they can be added after an action is closed to explain a decision.

<Shot name="human-form-default" alt="Approval drawer with an advisory-decision notice, the requested deployment, a note field and Deny and Approve buttons" caption="An approval request" />

::: warning Advisory unless a policy gates the operation
Approving or denying records the decision, notifies the requester and appears in the audit log. On its own, that
enforces nothing. When an approval policy gates an operation SpaceMan controls, the request is an
[enforced approval](./approvals): the operation runs only after approval, and only as approved. The drawer says which
kind you are deciding.
:::

## Custom types

Administrators add their own types under **Action types**, without code changes. A type has a **request schema** (what
is asked), a **response schema** (what the person fills in), both in JSON Schema 2020-12, its **outcomes** (how it can
end, for example *scheduled* or *declined*), a default priority and expiry, and optional form metadata: field order,
labels, help text and a widget from a fixed list. Schemas cannot load anything from outside, and the metadata accepts
plain text only, so a type can never run code in the UI.

<Shot name="human-types" alt="Deploy window type with request and response schemas, form metadata, outcomes scheduled and declined, priority high and a 24-hour expiry" caption="A custom type in the catalog" />

- Types are versioned like prompts: you edit a draft and **publish** an immutable version.
- A space enables a type under **Human actions → Types**: following the latest version or pinned to one, and
  optionally under its own label as a variant.
- People choose an enabled type when creating an action; agents find them with `spaceman_human_types`.
- SpaceMan checks every request and response against the schemas, and a response must end with one of the outcomes.
- An action keeps the version it was created with: publishing a new version never invalidates outstanding requests.
- Forms are generated from the schema. **Edit as JSON** is always available, and fields the form cannot show use the
  JSON editor.

<Shot name="human-form-custom" alt="A deploy window request with environment, change and rollback plan, and a response form with window start, duration and conditions, with Scheduled and Declined buttons" caption="Answering a custom type" />

::: warning Outcomes are records, not permissions
A custom outcome named *approved* is only a recorded answer. Custom types never grant anything; enforced approvals
are a separate mechanism.
:::

## Lifecycle

An action is **open**, **in progress** (someone claimed it), **resolved**, **cancelled** (withdrawn) or **expired**.
The lifecycle is separate from the **outcome**: a resolved approval is either *approved* or *denied*.

- **Responses are final.** Two people responding at the same time cannot both win: the first response is recorded, the
  second gets a conflict. To correct a response, create a follow-up action; the original stays unchanged.
- **Claim** assigns an action to yourself and marks it in progress; **Unassign** returns it to open.
- **Withdraw** cancels an action that is no longer needed.
- An action can have an **expiry**. When it passes, the action expires without an outcome.
- A repeated request with the same deduplication key returns the original action instead of creating a second one.

<Shot name="human-space" alt="A space's human actions: a blocking high-priority question, an approval, a to-do, an FYI message and an answered question" caption="A space's human actions, including a resolved one" />

## Blocking a task

An action can be tied to a task in the queue and **block** it. The task then waits ("waiting for human action …") and
the queue selects the next eligible task instead. Only that task is affected.

- A response or a withdrawal removes the blocker. The task keeps its state: control, or an operator, decides what
  happens next.
- An **expired** action leaves the task blocked, so a person decides.
- Informational messages never block. Only a message that needs an acknowledgement can.

## Requesters and notifications

The **requester** is set by SpaceMan from who is asking (an operator, an agent run, or SpaceMan itself), never from the
request's content. When an agent asked, the response (or a withdrawal or expiry) creates a durable notification,
stored in the same transaction as the response, so it survives restarts. Agents create and read human actions through
their SpaceMan tools and never wait in a loop: they are told when a response arrives.

- A **busy** requester gets a one-line terminal notice as soon as its program is idle again; an **idle** requester is
  woken by that notice at once (unless the policy turns notices off; then it reads the response the next time it checks
  `spaceman_messages`). The notice says only that something is waiting.
- When the requester's run has **ended** (it exited, failed, was lost, or ends before acknowledging the response), the
  policy decides:
  - **On at once** (`reroute`, the default): to a running run of the policy's **successor role**, else to the space's
    **control** (also while the supervisor restarts it), else nobody.
  - **To its next run** (`next_launch`): kept for the next run of the same agent instance, a resumed session such as a
    restarted control or a sleeping knowledge-base session, for the policy's hold time; then on as above.
- If no agent can take it, the action shows the response as **undeliverable**, operators get a message, and **Send to
  control** routes it once control runs.
- Every routing records the original run, the previous target and the reason, in the action's drawer, in the audit log
  and as a `human.action.rerouted` event. The run that receives a rerouted response is told whose it was.
- A response only ever reaches its target: the requester's run or agent instance, the run it was routed to, or
  control. Other runs of the space can read the request but not the response, comments or reason.

<Shot name="human-rerouted" alt="An answered question from a worker that had stopped: the notification went to the space's control, routed from the worker's run because that run had ended" caption="A response routed to control because its requester had ended" />

SpaceMan does not relaunch an ended worker to hand it a response: only control starts workers, so the response goes
to control, which decides.

Text written by an agent is untrusted: it is rendered as sanitized Markdown, never as HTML.

## Reminders, escalation and policies

Each action type has a **policy**. Installation defaults are under **Action types → Reminders, escalation and
routing**; a space overrides them under **Human actions → Policies**, for one type or for every type. The most specific
policy applies as a whole: the space's policy for the type, the space's policy for every type, the default for the
type, the default for every type, then the built-in policy.

<Shot name="human-policies" alt="A space's policies: approvals overridden in the space with reminders every two hours and escalation to urgent after four hours, to-dos reminded after a minute, and built-in policies for the other types" caption="Policies in force in a space, with where each comes from" />

| Built-in | Reminders | Escalation |
|---|---|---|
| Question | after 4 hours, then daily, 3 at most | after a day: priority high |
| Approval | after 1 hour, then every 8 hours, 3 at most | after 8 hours: priority high |
| To-do and custom types | once, after a day | never |
| Message | never | never |

- A **reminder** re-surfaces a pending action: it shows *reminded* (with a count) in Needs you and the space's list,
  and **Only reminded or escalated** narrows Needs you to those. The reminder does not change the action's version
  or its place in the order, so someone answering it is not interrupted.
- **Escalation** raises the priority (never lowers it) and/or assigns the action to a person, marks it *escalated* and
  is audited. The order of the list never changes; people decide it.
- Changing a policy recomputes the reminder and escalation times of pending actions at once.
- Expiry, reminders, escalation and holds run on one timer set to the earliest deadline, woken by every change.
  Nothing polls per action or per space.

## Bulk actions

Select actions to **Claim** them, or to **Acknowledge** messages, in one step. Answers, completions and approvals are
always made one at a time, with the request visible.

## Limits

A space holds at most 500 pending human actions, and one agent run at most 25, so a misbehaving agent cannot flood
the list. The policy also limits how many actions of a type agents may ask per hour: per run (built-in 30) and per
space (built-in 300). People are never rate limited. An agent over a limit gets a `rate_limited` error that tells it
waiting is enough, since it is notified of every response.
