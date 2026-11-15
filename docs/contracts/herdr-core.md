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

All spikes ran against an **isolated** core:
- `HC_SESSION=smspike`, with private `XDG_STATE_HOME` and `XDG_CONFIG_HOME`, and the `HC_TERMINAL_*` variables unset.
- `config.toml` sets `socket=<short path>` and `notes=false`.
- Run on 2026-11-02 with hc 0.1.16 on macOS. Scripts are kept out of the repo, and the procedure is reproduced in `test/real/hc_contract_test.go` (M1-06).

### S-0: socket path limit
`hc server start` with a long `XDG_STATE_HOME` failed:
`{"kind":"usage","message":"the socket path for session \"smspike\" is too long here (167 bytes; this system allows 103)"}`.
→ `spaceman-agent` always writes `config.toml` with an explicit short `socket` path in a 0700 directory, and checks its length (≤103 bytes
on macOS, ≤107 on Linux `sun_path`).

### S-1 (K-2): env persistence — **hc persists env in plaintext**
`terminal.run{env:{SM_SPIKE_CANARY:"canary-value-7731"}}` → `terminals.json` contained `"restart_env": {"SM_SPIKE_CANARY": "canary-value-7731"}`.
The file mode was **0644** inside a 0700 directory. The persisted record also had `program_started_ms`, `holder`, `scope` and `foreground`.
→ **Secrets must never be passed through `terminal.run.env`.** ADR-0009 (launcher shim reading a 0600 file it unlinks) is now required.

### S-2 (K-3): parent/child lifetime — **children end with the parent's program**
- Schema text: `parent` = "Parent terminal (default: the terminal it is started from)". `outlive_parent` = "Keep it running when its parent's
  program ends (by default it ends too)". `no_parent` = "No parent, even when started from a terminal". `die_with_parent` = "Ignored".
- Parent program **exited by itself** (`sleep 3`, exit 0) → child `state=finished, reason=killed, exit_code=129` (SIGHUP).
- Parent **killed** (`terminal.kill`) → plain child `reason=killed`. A child with `outlive_parent:true` stayed `live`.
→ A **lease-guard parent terminal** is a real dead-daemon mechanism:
  - The guard is a separate `spaceman-agent lease-guard` process inside hc. It reads a lease file that the daemon renews, and it exits
    when the lease expires. hc then ends every run terminal parented to it.
  - **Limits:**
    - It relies on the hc core and holder being alive. If hc itself dies, programs are lost anyway (`holder_lost`).
    - Ending a terminal's program does not prove the program's own descendants (processes it spawned outside hc) stopped (SM-SEC-001).
    - Every SpaceMan-launched terminal passes `parent` or `no_parent` explicitly, so it never inherits the caller's terminal as parent by accident.

### S-3 (K-1): framed protocol — **decoded from real `hc attach` traffic**
Method: `hc attach` (hc 0.1.16) was run under a PTY through a logging Unix-socket proxy against the isolated core, and an independent client was then written from the decoded layout.
- **Handshake.** The client sends the newline-delimited JSON `{"jsonrpc":"2.0","id":0,"method":"hello","params":{"framed":true}}`. The server replies with one JSON line.
  From then on both directions use frames.
- **Frame layout.**

  | Field | Encoding |
  |---|---|
  | channel | u32 little-endian |
  | kind | u8 |
  | length | u32 little-endian |
  | payload | `length` bytes |

  Channel `0` carries JSON-RPC requests and responses (kind 0). `terminal.attach` returns `{channel:N, terminal:{…}}`, and the stream then flows on
  channel N.
- **Kinds observed on an attach channel.**

  | Kind | Name | Direction | Payload |
  |---|---|---|---|
  | `0` | DATA | server→client | terminal output bytes |
  | `0` | DATA | client→server | keystrokes; ignored in `observe` mode |
  | `1` | CREDIT | client→server | u32 LE count of bytes consumed |
  | `2` | CLOSE | server→client | JSON `{"exit_code":3,"finished_ms":…,"reason":"exited","signal":null}` |
  | `3` | RESIZE | client→server | u16 LE cols, u16 LE rows |

  - CREDIT is flow control: the real client returns the size of each DATA frame it consumed.
  - FOCUS and FACTS were not observed, so their numbers are unknown and SpaceMan does not send them.
- **First paint.** The first DATA frame is a screen repaint: a reset of terminal modes, then the visible screen.
- **`observe:true`.** Input is not delivered (verified), and the terminal keeps its size: 200×120 stayed 200×120 with an observer at 40×10.
  An interactive (non-observe) attach **resizes the terminal** to its cols and rows (80×24 verified).
- **No sequence numbers.** The stream carries no byte offsets. After a reattach, the new first paint follows a gap that SpaceMan records
  explicitly.
- **Default size.** Terminals start at **200×120**, because `terminal.run` takes no size parameters.
→ ADR-0010 accepted: the recorder attaches with `observe:true` and writes DATA, CLOSE and `size` events (from `events.subscribe`) to zstd segments.

### S-4: details found by the real suite (`internal/runner/hc/hc_real_test.go`, 2026-11-02)
- `terminal.run` with `"labels": null` fails with `invalid_params: invalid type: null, expected a map`. Omit `labels` when there are none.
- `signal` (on the Terminal object and in CLOSE) is a **number**, e.g. SIGHUP = 1.
- **Framed event lines.** Each event is one newline-terminated JSON-RPC notification on the subscription channel:
  `{"jsonrpc":"2.0","method":"event","params":{"type":"finished","seq":37,"terminal":"t11","data":{…}}}`.
  For `created`, `data` is the full Terminal. For `finished`, it is `{exit_code, finished_ms, reason, signal}`. A `foreground` event type also exists.
  The `missed` event shape has **not** been observed: SpaceMan treats `type:"missed"` as a missed signal and re-snapshots, and that
  path is unverified.

### S-5: interactive attach for terminal takeover (SM-UI-004), and key names
Run on 2026-11-03 with hc 0.1.16 on macOS against an isolated core (own `HC_SESSION`, XDG directories and short `socket`,
`HC_TERMINAL_*` unset), by hand and in `internal/runner/hc` `TestRealTakeoverTypesAndDetaches` (`go test -tags real`).
- **Schema text (`hc schema`).** `terminal.attach` params: `terminal`, `cols`, `rows` (required), `history`, `observe` ("Watch only:
  never type or resize"), `resumed` ("Back after a dropped line: sets the size only once active again"), `display`, `caller`.
  `terminal.send`: "Paste text and press Enter, once nobody has typed for ~3 s; keys a person presses meanwhile are held, then
  delivered. Refuses a blocked program unless `force`." `terminal.keys`: "Press named keys, encoded for the program's current
  keyboard modes. Same quiet rule as send."
- **Interactive attach types bytes.** An attach with `observe:false` opened at the terminal's current size (200×120) on a
  connection of its own: DATA frames written by the client reach the program verbatim (`typed $HOME ~ "q" \x` then `\r`
  arrived as one line in `cat`), and the terminal keeps its size. After the client closes that connection the terminal still
  has its size, and a later `terminal.send` is delivered (about 3 s after the last typed key, the quiet rule).
- **No client detach frame is used.** A client-side CLOSE on an attach channel was not observed in real traffic (S-3), so
  SpaceMan detaches a takeover by closing its dedicated connection.
- **`terminal.keys` names.** Accepted: single letters and digits, `.`, `/`, `'`, `space`, `tab`, `enter`, `esc`/`escape`,
  `backspace`, `delete`, `up`/`down`/`left`/`right`, `home`, `end`, `pgup`, `pgdn`, `f1`…, `ctrl+<k>`, `alt+<k>`, `shift+<k>`,
  `ctrl+left`. Refused with `invalid_params` ("unknown key"): `"`, `\`, `$`, `~`. Keys cannot carry arbitrary text, so
  takeover types through the interactive attach rather than `terminal.keys`.
→ Terminal takeover (ADR-0046) uses an interactive attach on a dedicated connection, at the current size.

### S-6: idle-gated input and terminal size (2026-11-03, hc 0.1.16, isolated core)
- **`terminal.send` with `if` takes one exact status.** `hc send --help`: "Only type if the status is exactly this
  [possible values: idle, working]". A program reporting `done` is refused: `status_mismatch`, "t2 is done, not idle".
  Claude Code reports `done` after every turn and stays there until it gets input, so an `if: idle` gate alone refused
  every SpaceMan notice after a run's first turn. `internal/runner/hc.Runner.Send` treats `done` like `idle` (a
  finished turn waiting for input): when the gate refuses and `terminal.get` still reports `done`, it sends ungated;
  the core still waits ~3 s without human typing before pressing Enter. `TestRealSendIdleOrDone` (idle and done
  accepted, working refused).
- **Programs start at the core's `headless` size.** `terminal.run` takes no size (`hc schema`: `cols`/`rows` appear
  only in `Terminal` and `RunResult`). The default is 200×120. `config.toml` `headless = "COLSxROWS"` sets it (an
  invalid value lets the core start but its terminals fail to spawn). A running core uses a rewritten value for the
  programs it starts next, without a restart. The agent writes `runner.terminal_size` (default 160×48) there.
- **An interactive attach resizes only while attached.** At another size it resizes the program at once (the program's
  `stty size` changes); once the attach's connection closes, the program is back at the headless size. An observer
  never resizes. `TestRealTerminalSize`.

### S-7: holding a size, several interactive attaches and size events (2026-11-09, hc 0.1.17, isolated core)
Method: a temporary probe and then `internal/runner/hc` `TestRealTerminalSizeHold` (`go test -tags real`) against an
isolated core, with a program that prints `stty size` in a loop.
- **RESIZE frames on an open interactive attach** (kind 3, u16 LE cols, u16 LE rows, S-3) resize the program at once,
  as often as they are sent. The size lasts as long as that attach's connection is open.
- **The newest interactive attach sets the size.** With one attach held at 90×25, a second interactive attach (at the
  same size) that resizes to 70×20 sets 70×20. Closing it brings back 90×25. After all interactive attaches close,
  the program is back at the headless size.
- **`events.subscribe` reports every change** as `{"type":"size","terminal":"t1","data":{"cols":N,"rows":M}}`,
  including the fallback when an attach closes.
- **An open interactive attach that never writes does not hold back `terminal.send`.** With a hold open, a
  `terminal.send` was delivered about 1 s later (the quiet rule counts typed keys, S-5, S-6).
→ ADR-0075: the node holds a run's size through `runner.Takeover` plus RESIZE, never writes to it, and records sizes
from its own resizes and from these events.
