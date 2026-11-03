# Terminals and recordings

## Watching a run

Each run's page opens on its terminal. While the run is active, the **Live** view shows the stored output and then
streams new output as it happens. It is **watch-only**: key presses in the browser are never sent to the run. To give an
agent input, use **Message the agent**, which delivers the text the next time the agent is idle, or stop it.

<Shot name="terminal-live" alt="Live watch-only terminal of a running control agent with a message box below" caption="A live, watch-only terminal" />

Watching a terminal needs the `terminal:watch` permission (operator and above). The stream re-checks your session
periodically and ends if it was revoked or your grant was removed.

## Taking over a terminal

Sometimes an agent waits at a prompt that only a person at the keyboard can answer (a harness's own permission dialog,
an interactive tool). **Take over terminal** on a running run's page lets one person type into it:

- It needs the `terminal:takeover` permission (admins by default) and a recent sign-in. Operators see the takeover but
  cannot start one.
- While a terminal is taken over, every automated input to it waits: **Message the agent** is refused (and hidden),
  message and human-response notices are held and sent after the takeover ends, and commands already queued on the node
  are refused there. Stopping or interrupting the run still works.
- Everyone watching the run sees a banner naming who holds it. The holder's page renews the takeover every 10 seconds;
  it ends on **Release**, when another admin ends it, when the run ends, or 30 seconds after the holder's page stops
  renewing it (a closed tab, a revoked session or token). A revoked holder can no longer type at once.
- Start and end are audited (`run.takeover_started`, `run.takeover_ended`, with who, why it ended, how long and how many
  bytes were typed). The keystrokes themselves are never stored, logged or audited; the recording shows only what the
  program echoes, as for any output.
- Typed keys reach the program through herdr-core's interactive attach on the node (node protocol 1.6; older agents
  cannot be taken over). The terminal keeps its size rather than fitting your browser window.

<Shot name="terminal-takeover" alt="A run's live terminal taken over by the signed-in admin: a banner says you have taken over this terminal, with a Release button" caption="A taken-over terminal: automated input waits until it is released" />

From the command line: `spaceman run takeover start|type|renew|end` ([CLI reference](/reference/cli/run)).

## Replay

**Replay** plays the stored recording with play and pause, 1× to 8× speed and seeking. Long idle stretches are shortened
to two seconds. The badges show the terminal size, gaps (output that may be missing because the recorder re-attached),
truncation (the recording reached its size limit) and how the program ended. **Download** returns the recording as a
zstd-compressed JSON Lines file.

<Shot name="terminal-replay" alt="Replay of a finished run with restart, play, speed selector, seek bar and download" caption="Replaying a finished run" />

Reading recordings needs `recording:read` (operator and above); deleting central's copy needs `recording:delete` (admin)
and is possible once the run has ended. Each recording is bounded by the space's `recording_quota_bytes` limit (default
512 MiB), and the space's `retention_recordings_days` deletes central's copy that many days after the run ended; both
are on the space's Limits page ([Retention and export](/guide/retention)). Deletions are audited.

## How recording works

```mermaid
flowchart LR
  H["herdr-core terminal"] -->|"observe-only attach"| R["agent recorder"]
  R -->|"zstd segments"| L[("node: runs/RUN/recording")]
  R -->|"records, once a second"| C["central"]
  C -->|"stored copy"| S[("central: recordings/RUN")]
  S -->|"SSE / replay / download"| B["browser"]
```

- The agent attaches to the run's terminal as an observer, so recording never resizes the terminal or sends it input.
- Output is written to segments of zstd frames, one frame per flush; a segment cut off by a crash is readable up to its
  last complete frame.
- Records are numbered. Central stores each number once, so a re-sent batch after a reconnect is harmless, and the agent
  catches central up from its local copy after an outage.
- Each browser reads from central with its own bounded queue. A viewer that falls behind gets a gap marker and re-reads
  from storage; it never slows the node or the run.

herdr-core 0.1.16 terminal streams carry no byte offsets, so when the recorder has to re-attach, SpaceMan records an
explicit gap and the screen repaint that follows; output written while it was detached is not recoverable.

Recordings can contain anything that appeared in the terminal, including secrets an agent printed. Treat them as
sensitive.
