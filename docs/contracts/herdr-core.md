# Contract: herdr-core (`hc`)

- **Revision inspected:** `hc 0.1.16` binary (`~/.local/bin/hc` → `herdr-core`, Rust, Apache-2.0). No source checkout was available.
  Sources: `hc --help`, `hc schema` (OpenRPC 1.3.2), `hc --skill`, the live `hc get` output on the operator's machine. Inspected 2026-11-02.
- **Upstream:** https://github.com/herdrdev/herdr-core

## Transport (verified from schema `info`)
- JSON-RPC 2.0, one message per line on the hc socket, or framed after `hello{framed:true}`. Protocol generation 1. Errors carry a stable `data.kind`.
- Socket path: `$XDG_STATE_HOME/herdr-core/<session>/<session>.sock`. `HC_SESSION`, `HC_SOCKET` and the config `socket` key override it.
- `hc connect [--session S]` speaks the same protocol over stdin/stdout.
- Inside a terminal, hc sets `HC_TERMINAL_ID` and `HC_TERMINAL_SOCKET`.
- Process model: `herdr-core serve` (the core) plus `herdr-core hold` holders that own the PTYs. `server.restart` saves screens, and the
  next core adopts programs that are still running. `server.stop` stops the core *and its programs*.

## Methods SpaceMan uses (verified signatures)
| Method | Params (subset) | Result / notes |
|---|---|---|
| `hello` | `generation?, framed?` | `{generation, version, core{id,name}, framed, methods[], features[]}`; error `unsupported_generation` |
| `terminal.run` | `argv[] \| line, cwd, env{}, labels{}, name (a-z0-9-_, 1–32, unique among live), keep, rm, parent, no_parent, outlive_parent, no_notify, wait, timeout` | Terminal; errors `name_taken, spawn_failed, unavailable, invalid_params, …`. **No cols/rows.** |
| `terminal.list` | `all?, labels{}` (AND match) | `{terminals[], hidden_finished, machines[]}` |
| `terminal.get` / `terminal.kill` | `terminal` | Terminal. Kill sends HUP→TERM→KILL. |
| `terminal.rm` | `terminal` | removes a finished terminal |
| `terminal.label` | `terminal, labels` (`key=` removes) | |
| `terminal.read` | `terminal, lines` | `{text, complete, how: screen\|history\|stitched}`. Text only, no offsets. |
| `terminal.attach` | `terminal, cols, rows, history?, observe?` | **framed only**: `{channel, terminal}`; DATA/RESIZE/FOCUS/FACTS/CREDIT/CLOSE frames (layout **unverified**, K-1) |
| `terminal.send` | `terminal, text, force?, if?: idle\|working, wait?, timeout?` | pastes the text and presses Enter after ~3 s without human typing; `{turn, started, status}`; errors `blocked, status_mismatch, finished` |
| `terminal.keys` | `terminal, keys[]` | e.g. `enter`, `esc`, `ctrl+c` |
| `terminal.interrupt` | `terminal` | Esc if the program reports status, else Ctrl+C; `outcome: stopped\|exited\|still_working\|unknown` |
| `terminal.wait` | `terminal?, children?, output?, timeout?` | `{ended, reason, status, turn, exit_code}` |
| `events.subscribe` | `terminal?, children_of?, labels{}` | `{seq, terminals[] snapshot}`, then `event` notifications: created, lost, restarted, keep, finished, removed, status, turn, title, labels, size, doc. 15 s `heartbeat`. `missed` brings a fresh snapshot. |
| `doc.get` / `doc.set` | key, value, version | CAS JSON KV (≤1 MiB) |

Terminal object: `address` (permanent on that core), `name`, `labels`, `argv`, `cwd`, `pid`, `foreground{pid,…}`, `state live|finished`,
`created_ms`, `finished_ms`, `exit_code`, `signal`, `reason` (`exited, killed, server_stop, host_shutdown, signaled, holder_lost,
stopped_by_restart, spawn_pending`), `status{state,app,kind,msg,progress,title}`, `turn`, `reported`, `cols`, `rows`, `screen exact|inexact`.
**There is no process start-time field.** SpaceMan's identity tuple is `core.id + address + created_ms + pid`.

## Agent status (verified)
- Programs report status via OSC 7501. States are `idle, working, done, blocked, error`, and kinds are `permission, question, auth`.
- **Claude Code 2.1.316** probes `OSC 7501 ; ?` at startup and reports natively. Live `hc get` showed `status.app=claude-code` with `reported:true`.
- **Codex 0.163.0** contains the `]7501;state=` emitter strings. The hc manual says Codex must run with `--no-daemon`, or its commands run outside hc.
- herdr-core never infers status from screen text, and neither does SpaceMan.

## Open verification items
- K-1: framed byte layout.
- K-2: whether `env` is persisted in `terminals.json`.
- K-3: `parent` / `outlive_parent` kill semantics. Results go in the section below as spikes complete.

## Spike results
(none yet)
