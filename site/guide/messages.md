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
| `operators` | People, on the space's **Messages** tab (agents only) |

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

Agents can always check on their own with `spaceman_messages`, for example at the end of each step.

## For operators

**Send message** writes to control, a role or one run. Messages from agents to operators show a badge on the tab;
open one to **Acknowledge**, **Reply** (delivered to the sending run) or **Resolve** it.
