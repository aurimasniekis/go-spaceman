# Agent tools

Every Claude Code or Codex run gets exactly one MCP server: **SpaceMan**. It runs as `spaceman-agent mcp-bridge` next to
the harness and forwards each call over the node's authenticated connection to central. Central checks the run's
credential on every call, so a run that ended (or was relaunched) loses its tools at once.

What a run may do depends on its **kind**, not on anything the model says. The space, the run and the requester always
come from the run's credential; tool arguments can never name another space.

Runs of a role granted an external MCP server also see that server's tools in the same SpaceMan server, named
`<server>__<tool>`; see [MCP gateway](./mcp-gateway).

Agents ask people with `spaceman_human_ask`, never through an interactive terminal prompt: nobody may be watching the
run's terminal, so the run would wait silently. Every run's orientation says so, and Claude Code runs don't get Claude
Code's own question picker (`AskUserQuestion`) or plan mode (`EnterPlanMode`); see [Human actions](./human-actions#agents-ask-here-not-in-the-terminal).

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
| `spaceman_task_create` | Add a task (optionally from an inbox item); an incident needs a trusted trigger, else it is proposed or refused | ✓ | | | |
| `spaceman_task_resume` | Resume a task an incident preempted | ✓ | | | |
| `spaceman_task_set_status` | Move a task; a worker only its own task, to review or blocked | ✓ | own | | |
| `spaceman_attachment_get` | Fetch a `spaceman://attachment/<id>` (text, or base64 up to 512 KiB) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_ask` | Ask a person: question, approval, to-do or message | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_types` | Custom human-action types enabled in the space, with schemas | ✓ | ✓ | ✓ | ✓ |
| `spaceman_kb_list` | Knowledge bases the run may ask | ✓ | ✓ | ✓ | ✓ |
| `spaceman_kb_ask` | Ask a KB a question; returns at once, the answer arrives as a notice | ✓ | ✓ | ✓ | ✓ |
| `spaceman_kb_get` | Read a KB request: state, answer, basis and evidence | ✓ | ✓ | ✓ | ✓ |
| `spaceman_kb_cancel` | Withdraw a KB question | ✓ | ✓ | ✓ | ✓ |
| `spaceman_kb_answer` | (KB sessions) Answer a request with evidence | | | ✓ | |
| `spaceman_kb_propose` | (KB sessions) Offer a commit for acceptance | | | ✓ | |
| `spaceman_dream_report` | (Dreamers) Finish a dream: summary, changes, findings, or no change | | | dreamer | |
| `spaceman_memory_status` | The role memory's sync state: preserved history, pending changes, conflicts | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_get` | Read a human action, and its response when you asked it or it was routed to you | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_list` | List human actions (by default your own) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_reply` | Answer a clarification request on an action you asked (not a response; the action stays pending) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_human_cancel` | Withdraw a pending action you asked (control: any) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_messages` | Read waiting messages and human responses | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_send` | Message control, operators, a role or a run; control can schedule a reminder to itself with `deliver_at` or `delay` ([Scheduled messages](./messages#scheduled-messages)) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_scheduled` | List the scheduled messages to control that are not yet due | ✓ | | | |
| `spaceman_message_cancel` | Cancel a scheduled message before it is due | ✓ | | | |
| `spaceman_message_reply` | Reply to a message (resolves a request) | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_ack` | Acknowledge a message or human response | ✓ | ✓ | ✓ | ✓ |
| `spaceman_message_resolve` | Close a request you sent or received | ✓ | ✓ | ✓ | ✓ |
| `spaceman_runs_list` | Active runs with role, status and task | ✓ | ✓ | ✓ | ✓ |
| `spaceman_repos_list` | The space's configured repositories with the node's last report: remote, default branch, last fetch, changes, unpushed commits, worktrees and their runs; read only ([Repositories and worktrees](./repositories)) | ✓ | ✓ | KB | ✓ |
| `spaceman_worker_launch` | Start a worker for a ready, eligible task (optionally in its own git worktree of a repository, by name or folder) | ✓ | | | |
| `spaceman_worker_stop` | Stop a worker, KB, dreamer or diagnostic run | ✓ | | | |
| `spaceman_xspace_discover` | Spaces that let yours discover them, with what you may send ([Cross-space requests](./cross-space)) | ✓ | | | |
| `spaceman_xspace_request` | Ask another space: a question, delegated work or a KB question; returns at once, outcomes arrive as notices | ✓ | | | |
| `spaceman_xspace_get` | Read a request your space sent or received (replies as the other space discloses them) | ✓ | | | |
| `spaceman_xspace_list` | Your space's open (or all) outgoing and incoming requests | ✓ | | | |
| `spaceman_xspace_accept` | Accept a request another space sent, optionally creating your own task | ✓ | | | |
| `spaceman_xspace_decline` | Decline a pending request another space sent | ✓ | | | |
| `spaceman_xspace_reply` | Answer, or report the outcome (completed or failed); reduced to your disclosure level | ✓ | | | |
| `spaceman_xspace_cancel` | Withdraw an open request your space sent | ✓ | | | |

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
