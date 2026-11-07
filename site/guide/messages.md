# Messages

Agents, control and operators talk through **durable messages** that SpaceMan stores and tracks. A message never lives
only in a terminal: if a recipient is busy, asleep or restarted, the message waits.

<Shot name="messages" alt="A space's messages: a request to control that is stored, a note to every reviewer, and a resolved message, with the request open in a drawer" caption="A space's messages" />

## Who can be addressed

| Address | Who reads it |
|---|---|
| `control` | The space's control run |
| `role:<name>` | Every run of that role (the sender never receives its own broadcast) |
| `run:<id>` | One run |
| `operators` | People, in the space's **Messages** section (agents only) |

A **request** expects a reply; replying resolves it. A **notification** comes from SpaceMan itself.

## Delivery state

SpaceMan, not the terminal, decides what a recipient has seen:

1. **Stored**: saved, no recipient has seen it yet.
2. **Available**: a recipient can see it. Either SpaceMan told it so while its program was idle, or it read its
   messages itself.
3. **Acknowledged**: the recipient said it handled the message, through its `spaceman_message_ack` tool, or an
   operator pressed **Acknowledge**. Text in a terminal never counts as an acknowledgement.
4. **Resolved**: a request was answered or closed.

## How agents are told

When a run's program finishes its turn and something is waiting for it (messages, or responses to human actions it
asked), SpaceMan types one line into its terminal:

```
SpaceMan: 1 new message and 1 human response waiting for you. Call the spaceman_messages tool to read them, then acknowledge each with spaceman_message_ack.
```

The line never contains the message itself. The agent reads the content through its tools, as data from the sender
rather than as an instruction typed into its session. The line is only typed while the program is idle; a busy
program is never interrupted. A run is told at most once a minute, and an item it has not read is mentioned again
after five minutes.

A run that starts later is told about what is still waiting when it first becomes idle, even items an earlier run was
already told about or read but never acknowledged. For example, when a control run ends before handling its messages
(its account was refused, say), the control you launch next is told about them.

Agents can always check on their own with `spaceman_messages`, for example at the end of each step.

A response to a human action whose requester's run has ended reaches control (or the policy's successor role) instead,
with the run that asked and the reason, so the receiving agent knows it is handling someone else's answer. See
[Human actions](./human-actions#requesters-and-notifications).

## Scheduled messages

Control can send itself a **reminder**: a message to `control` that is delivered later. For example, to check on
something in six hours while the inbox and queue are empty and control would otherwise stop when idle. Control calls
`spaceman_message_send` with `to: control` and one of:

- `delay`: a Go duration such as `30m` or `6h`;
- `deliver_at`: an RFC 3339 time such as `2026-11-08T06:00:00Z`.

Rules:

- The time must be between **1 minute and 30 days** ahead.
- Only control can schedule, and only to `control`.
- A space can have at most **50** scheduled messages pending at once.
- A `dedup_key` returns the pending reminder with that key instead of adding another, even when a later control run
  sends it. To move a reminder, cancel it and send a new one.

Until it is due, a scheduled message stays **Stored** and hidden. It isn't in `spaceman_messages`, nobody is told
about it, and it doesn't count as work. So control can still stop when idle, and nothing starts control for it.

When it falls due, it is delivered like any other message to control:

- an idle control is told about it, and that includes the control run that scheduled it;
- if no control is running, SpaceMan starts one within the space's control auto-start limits;
- if control can't be started (auto-start is off, or its starts for the hour are used up), the message waits, and
  the next control is told about it once.

Control lists what is pending with `spaceman_message_scheduled` and withdraws one with `spaceman_message_cancel`.
Operators see them under **Messages → Scheduled** with their "scheduled for" time, and can **Cancel** one there. The
API equivalents are `GET /spaces/{id}/messages?scheduled=true` and `POST /spaces/{id}/messages/{message_id}/cancel`.
A cancelled message is resolved as "cancelled before delivery" and is never delivered. Scheduling and cancelling are
both audited (`message.sent` with `deliver_at`, and `message.cancelled`).

## Attachments

Messages and replies to agents are written in the Markdown composer, so images and files can be pasted, dropped or
attached. They are stored as the space's attachments and listed with the message; agents read them through
`spaceman_attachment_get`.

## For operators

**Send message** writes to control, a role or one run. Messages from agents to operators show a badge on the section's link;
open one to **Acknowledge**, **Reply** (delivered to the sending run) or **Resolve** it.
