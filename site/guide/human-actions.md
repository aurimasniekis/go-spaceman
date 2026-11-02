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

::: warning Approvals are advisory in this version
Approving or denying records the decision, notifies the requester and appears in the audit log. SpaceMan does not yet
enforce the decision on the operation itself. Enforced approvals, bound to the exact operation that was approved, arrive
with the approval gateway.
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
request's content. When an agent asked, the response creates a durable notification for that run, so it is not lost if
the agent is busy or asleep. Agents create and read human actions through their SpaceMan tools.

Text written by an agent is untrusted: it is rendered as sanitized Markdown, never as HTML.

## Bulk actions

Select actions to **Claim** them, or to **Acknowledge** messages, in one step. Answers, completions and approvals are
always made one at a time, with the request visible.

## Limits

A space holds at most 500 pending human actions, and one agent run at most 25, so a misbehaving agent cannot flood
the list.
