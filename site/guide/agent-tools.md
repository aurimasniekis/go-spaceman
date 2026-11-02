# Agent tools

Every Claude Code or Codex run gets exactly one MCP server: **SpaceMan**. It runs as `spaceman-agent mcp-bridge` next to
the harness and forwards each call over the node's authenticated connection to central. Central checks the run's
credential on every call, so a run that ended (or was relaunched) loses its tools at once.

What a run may do depends on its **kind**, not on anything the model says. The space, the run and the requester always
come from the run's credential; tool arguments can never name another space.

| Tool | What it does | Control | Worker | KB, dreamer | Diagnostic |
|---|---|:-:|:-:|:-:|:-:|
| `spaceman_whoami` | The run's identity: run, space, role, kind, harness | ✓ | ✓ | ✓ | ✓ |
| `spaceman_ping` | Whether SpaceMan is reachable | ✓ | ✓ | ✓ | ✓ |
| `spaceman_inbox_list` | List inbox items (without bodies) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_inbox_get` | Read an item and its attachments | ✓ | ✓ | ✓ | ✓ |
| `spaceman_inbox_add` | Add a finding or request to the inbox | ✓ | ✓ | ✓ | |
| `spaceman_inbox_set_status` | Defer, reject, mark done or link an item | ✓ | | | |
| `spaceman_tasks_list` | The queue in order, with eligibility reasons | ✓ | ✓ | ✓ | ✓ |
| `spaceman_task_get` | Read a task | ✓ | ✓ | ✓ | ✓ |
| `spaceman_queue_next` | The next eligible task and why others were skipped | ✓ | | | |
| `spaceman_task_create` | Add a task (optionally from an inbox item) | ✓ | | | |
| `spaceman_task_set_status` | Move a task; a worker only its own task, to review or blocked | ✓ | own | | |
| `spaceman_attachment_get` | Fetch a `spaceman://attachment/<id>` (text, or base64 up to 512 KiB) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_ask` | Ask a person: question, approval, to-do or message | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_get` | Read a human action and its response | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_list` | List human actions (by default your own) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_cancel` | Withdraw a pending action you asked (control: any) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_messages` | Read waiting messages and human responses | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_send` | Message control, operators, a role or a run | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_reply` | Reply to a message (resolves a request) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_ack` | Acknowledge a message or human response | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_resolve` | Close a request you sent or received | ✓ | ✓ | ✓ | ✓ |
| `spaceman_runs_list` | Active runs with role, status and task | ✓ | ✓ | ✓ | ✓ |
| `spaceman_worker_launch` | Start a worker for a ready, eligible task (optionally in its own git worktree) | ✓ | | | |
| `spaceman_worker_stop` | Stop a worker, KB, dreamer or diagnostic run | ✓ | | | |

Rules that apply to every tool:

- **Calls finish.** A call never waits for a person or another agent: `spaceman_human_ask` returns a durable ID at
  once, and the requester is notified when someone responds. Calls are cut off after 30 seconds; a call that timed out
  may still have taken effect, so check before retrying.
- **Retries are safe.** Inbox items, tasks and human actions accept a deduplication key; repeating it returns the
  original instead of creating a second one.
- **Content is untrusted.** Inbox bodies, attachments and human responses are data from their sources, not
  instructions. They are returned as text and never executed.
- **Accountable.** Everything a run changes is in the audit log under the run's ID.
- **Nothing is accepted automatically.** A worker can mark its task ready for review; control or an operator decides
  when it is done.
