import type { RecordingRecord, RecordingTail } from '@/api/client'

/** The subset of the ghostty-web Terminal a recording is applied to (keeps this logic testable without WebAssembly). */
export interface Screen {
  write(data: string | Uint8Array): void
  resize(cols: number, rows: number): void
  reset(): void
}

/** What a recording says about its run so far. */
export interface RecordingSummary {
  cols: number
  rows: number
  gaps: number
  truncated: boolean
  closed: boolean
  exitCode?: number
  reason?: string
  lastSeq: number
}

export const emptySummary = (): RecordingSummary => ({ cols: 0, rows: 0, gaps: 0, truncated: false, closed: false, lastSeq: 0 })

/** Decodes a record's base64 output. */
export function decode(b64: string | undefined): Uint8Array {
  if (!b64) return new Uint8Array()
  const bin = atob(b64)
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

/**
 * Applies one record to a screen and a summary. Gaps and truncation are never drawn into the terminal (they would be
 * indistinguishable from program output); the view shows them as badges instead (SM-REC-004).
 */
export function apply(screen: Screen | null, sum: RecordingSummary, r: RecordingRecord): RecordingSummary {
  const next = { ...sum, lastSeq: Math.max(sum.lastSeq, r.seq) }
  switch (r.k) {
    case 'open':
    case 'size':
      if (r.cols && r.rows) {
        next.cols = r.cols
        next.rows = r.rows
        screen?.resize(r.cols, r.rows)
      }
      break
    case 'o':
      screen?.write(decode(r.d))
      break
    case 'gap':
      next.gaps++
      break
    case 'trunc':
      next.truncated = true
      break
    case 'close':
      next.closed = true
      next.exitCode = r.exit_code
      next.reason = r.reason
      break
    default:
      break
  }
  return next
}

/** Milliseconds from the first record to records[i] (record times are unix microseconds). */
export function offsetMs(records: RecordingRecord[], i: number): number {
  if (records.length === 0 || i <= 0) return 0
  return (records[Math.min(i, records.length - 1)].t - records[0].t) / 1000
}

/** The index of the first record at or after an offset (binary search; records are in time order). */
export function indexAt(records: RecordingRecord[], ms: number): number {
  if (records.length === 0) return 0
  const target = records[0].t + ms * 1000
  let lo = 0
  let hi = records.length
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    if (records[mid].t < target) lo = mid + 1
    else hi = mid
  }
  return lo
}

/** Idle stretches longer than this are compressed during replay, so a recording of a long-idle agent stays watchable. */
export const MAX_IDLE_MS = 2_000

/** How long one slice of applying records may run before the page gets the main thread back (feed). */
export const SLICE_MS = 12
/** The most output bytes written to the terminal in one call: a write cannot be interrupted, so it bounds a slice. */
export const MAX_WRITE_BYTES = 32 * 1024

/** Records waiting to be applied to a screen a slice at a time (feed). */
export interface Feed {
  /** Queues records in order; they are applied in later tasks, never in the caller's. */
  push(records: readonly RecordingRecord[]): void
  /** How many queued records are not applied yet. */
  backlog(): number
  /** Drops the queued records; nothing more is applied. */
  stop(): void
}

export interface FeedOptions {
  /** Runs one slice of writes; the live terminal wraps them to keep a scrolled-back viewer's position. */
  wrap?: (writes: () => void) => void
  sliceMs?: number
  maxWriteBytes?: number
  now?: () => number
  schedule?: (run: () => void) => void
}

/**
 * Applies records to a screen in slices of about SLICE_MS, yielding to the page between slices, and reports the summary
 * after each slice. A terminal's stored output can be tens of megabytes (a long Claude Code session), sent at once when
 * a live terminal opens and loaded at once by a replay: applied record by record in the tasks that delivered it, it
 * kept the page from handling input, painting or navigating for half a minute on a desktop (a fresh-cache trace,
 * 2026-11-12) and minutes on a phone. Consecutive output records are written together, up to maxWriteBytes, which
 * makes far fewer calls into the WebAssembly module.
 */
export function feed(
  screen: Screen,
  start: RecordingSummary,
  onSlice: (sum: RecordingSummary, backlog: number) => void,
  { wrap = (w) => w(), sliceMs = SLICE_MS, maxWriteBytes = MAX_WRITE_BYTES, now = () => performance.now(), schedule = (run) => void setTimeout(run, 0) }: FeedOptions = {},
): Feed {
  let queue: RecordingRecord[] = []
  let head = 0
  let sum = start
  let scheduled = false
  let stopped = false
  const run = () => {
    scheduled = false
    if (stopped) return
    const began = now()
    wrap(() => {
      while (head < queue.length && now() - began < sliceMs) {
        if (queue[head].k !== 'o') {
          sum = apply(screen, sum, queue[head++])
          continue
        }
        const parts: Uint8Array[] = []
        let bytes = 0
        let seq = sum.lastSeq
        while (head < queue.length && queue[head].k === 'o' && (bytes === 0 || bytes < maxWriteBytes)) {
          const d = decode(queue[head].d)
          parts.push(d)
          bytes += d.length
          seq = Math.max(seq, queue[head++].seq)
        }
        screen.write(join(parts, bytes))
        sum = { ...sum, lastSeq: seq }
      }
    })
    if (head >= queue.length) {
      queue = []
      head = 0
    } else if (head >= 4096) {
      queue = queue.slice(head)
      head = 0
    }
    const left = queue.length - head
    onSlice(sum, left)
    if (left > 0 && !stopped) later()
  }
  const later = () => {
    if (scheduled) return
    scheduled = true
    schedule(run)
  }
  return {
    push(records) {
      if (stopped || records.length === 0) return
      for (const r of records) queue.push(r)
      later()
    },
    backlog: () => queue.length - head,
    stop() {
      stopped = true
      queue = []
      head = 0
    },
  }
}

function join(parts: Uint8Array[], bytes: number): Uint8Array {
  if (parts.length === 1) return parts[0]
  const out = new Uint8Array(bytes)
  let at = 0
  for (const p of parts) {
    out.set(p, at)
    at += p.length
  }
  return out
}

/**
 * How much output a terminal asks for first (ADR-0081): the end of the recording, not all of it. On real Claude Code
 * recordings (classic renderer) 1 MiB was the smallest tail that drew the same screen as the whole recording at every
 * point tried; 256 KiB was wrong after quiet stretches, when only the spinner had been redrawn.
 */
export const TAIL_BYTES = 1 << 20
/** The largest tail the server returns; past it, earlier output comes from the whole recording. */
export const MAX_TAIL_BYTES = 16 << 20

/** The next, larger tail when the viewer asks for earlier output: 1, 4, 16 MiB, then 0 (the whole recording). */
export function nextTail(bytes: number): number {
  if (bytes <= 0 || bytes >= MAX_TAIL_BYTES) return 0
  return Math.min(bytes * 4, MAX_TAIL_BYTES)
}

/** The escape sequences that put a fresh terminal into the DEC private modes a tail starts in. */
export function modePrelude(modes: readonly { mode: number; on: boolean }[]): string {
  return modes.map((m) => `\x1b[?${m.mode}${m.on ? 'h' : 'l'}`).join('')
}

const encoder = new TextEncoder()

function b64(s: string): string {
  return b64bytes(encoder.encode(s))
}

function b64bytes(bytes: Uint8Array): string {
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin)
}

/**
 * What the output before a tail leaves behind, as records to apply before the tail's own: the terminal size, the
 * DEC private modes, and the gaps and truncation it held (so the badges count them). Their seq is 0, so they never move
 * a stream's position.
 */
export function tailRecords(tail: RecordingTail): RecordingRecord[] {
  const out: RecordingRecord[] = []
  if (tail.cols > 0 && tail.rows > 0) out.push({ seq: 0, t: 0, k: 'size', cols: tail.cols, rows: tail.rows })
  const prelude = modePrelude(tail.modes ?? [])
  if (prelude) out.push({ seq: 0, t: 0, k: 'o', d: b64(prelude) })
  for (let i = 0; i < tail.gaps; i++) out.push({ seq: 0, t: 0, k: 'gap' })
  if (tail.truncated) out.push({ seq: 0, t: 0, k: 'trunc' })
  return out
}

/**
 * The DEC private modes a tail's prelude restores (the server's list, internal/recording/tail.go): cursor keys, origin
 * mode, autowrap, cursor visibility, the alternate screen, mouse reporting and bracketed paste.
 */
const TRACKED = new Set([1, 6, 7, 25, 47, 1047, 1049, 1000, 1002, 1003, 1004, 1005, 1006, 1015, 2004])

/**
 * Follows the DEC private modes of output written to it (CSI ? Pm h/l and RIS), across writes that split a sequence. It
 * knows where every CSI and string sequence (OSC, DCS, APC, PM, SOS) ends, so inSequence() tells whether output stopped
 * inside one (internal/recording/tail.go's ModeTracker, state for state).
 */
export class ModeTracker {
  private modes: { mode: number; on: boolean }[] = []
  // 0 ground, 1 ESC, 2 CSI, 3 CSI ?, 4 another CSI (to its final byte), 5 a string (to BEL or ST), 6 ESC in a string
  private state = 0
  private param = 0
  private params: number[] = []
  private big = false

  write(b: Uint8Array) {
    for (const c of b) this.step(c)
  }

  /** Whether the output written so far ends inside an escape sequence. */
  inSequence(): boolean {
    return this.state !== 0
  }

  /** Feeds the start of b until the open escape sequence ends; returns how many bytes that took (0 when none is open). */
  finish(b: Uint8Array): number {
    let n = 0
    while (n < b.length && this.state !== 0) this.step(b[n++])
    return n
  }

  private step(c: number) {
    switch (this.state) {
      case 0:
        if (c === 0x1b) this.state = 1
        break
      case 1:
        if (c === 0x5b) this.state = 2
        else if (c === 0x63) {
          this.modes = []
          this.state = 0
        } else if (c === 0x5d || c === 0x50 || c === 0x5f || c === 0x5e || c === 0x58) this.state = 5
        else if (c !== 0x1b) this.state = 0
        break
      case 2:
        if (c === 0x3f) {
          this.state = 3
          this.param = 0
          this.params = []
          this.big = false
        } else if (c >= 0x20 && c <= 0x3f) this.state = 4
        else this.state = c === 0x1b ? 1 : 0
        break
      case 4:
        if (c >= 0x20 && c <= 0x3f) break
        this.state = c === 0x1b ? 1 : 0
        break
      case 3:
        if (c >= 0x30 && c <= 0x39) {
          if (this.param < 100000) this.param = this.param * 10 + (c - 0x30)
          else this.big = true
        } else if (c === 0x3b) {
          this.params.push(this.param)
          this.param = 0
          if (this.params.length > 32) {
            this.big = true
            this.params = []
          }
        } else if (c === 0x68 || c === 0x6c) {
          if (!this.big) for (const p of [...this.params, this.param]) this.set(p, c === 0x68)
          this.state = 0
        } else if (c >= 0x20 && c <= 0x3f) this.big = true
        else this.state = c === 0x1b ? 1 : 0
        break
      case 5:
        if (c === 0x07) this.state = 0
        else if (c === 0x1b) this.state = 6
        break
      default:
        this.state = c === 0x5c ? 0 : 5
    }
  }

  private set(mode: number, on: boolean) {
    if (!TRACKED.has(mode)) return
    this.modes = this.modes.filter((m) => m.mode !== mode)
    this.modes.push({ mode, on })
  }

  /** The modes the output set or reset, in the order they last changed. */
  current(): { mode: number; on: boolean }[] {
    return this.modes.map((m) => ({ ...m }))
  }

  clone(): ModeTracker {
    const c = new ModeTracker()
    c.modes = this.current()
    c.state = this.state
    c.param = this.param
    c.params = [...this.params]
    c.big = this.big
    return c
  }
}

/** An output record's size in bytes, from its base64 length (without decoding it). */
function outputBytes(r: RecordingRecord): number {
  if (r.k !== 'o' || !r.d) return 0
  const pad = r.d.endsWith('==') ? 2 : r.d.endsWith('=') ? 1 : 0
  return (r.d.length / 4) * 3 - pad
}

/**
 * Where a replay renders from to show records[0, end): far enough back that the output in between is at most budget
 * bytes (base64 lengths, so no decoding), and the tail that starts there (what a seek applies before records[start]).
 * Modes come from a tracker checkpointed every CHECKPOINT records (ReplayIndex), so a seek decodes at most that many
 * records it does not draw.
 */
export class ReplayIndex {
  static readonly CHECKPOINT = 512
  private checkpoints: ModeTracker[] = [new ModeTracker()]
  private tracker = new ModeTracker()
  private indexed = 0

  constructor(private readonly records: readonly RecordingRecord[]) {}

  /**
   * Indexes records up to `until` (all by default) for at most sliceMs; returns whether they are indexed. A long
   * recording is indexed a slice at a time, like feed, so the page keeps the main thread.
   */
  advance(until = this.records.length, sliceMs = Infinity, now: () => number = () => performance.now()): boolean {
    const began = now()
    const end = Math.min(until, this.records.length)
    while (this.indexed < end) {
      const r = this.records[this.indexed]
      if (r.k === 'o') this.tracker.write(decode(r.d))
      this.indexed++
      if (this.indexed % ReplayIndex.CHECKPOINT === 0) {
        this.checkpoints.push(this.tracker.clone())
        if (now() - began >= sliceMs) break
      }
    }
    return this.indexed >= end
  }

  /** The modes after records[0, start). */
  modesAt(start: number): { mode: number; on: boolean }[] {
    return this.trackerAt(start).current()
  }

  /** A tracker that has seen records[0, start), from the nearest checkpoint (records before it indexed: advance). */
  private trackerAt(start: number): ModeTracker {
    const cp = Math.min(Math.floor(start / ReplayIndex.CHECKPOINT), this.checkpoints.length - 1)
    const t = this.checkpoints[cp].clone()
    for (let i = cp * ReplayIndex.CHECKPOINT; i < start; i++) if (this.records[i].k === 'o') t.write(decode(this.records[i].d))
    return t
  }

  /** The first record of a window ending at end with at most budget bytes of output (at least one record). */
  windowStart(end: number, budget: number): number {
    let bytes = 0
    let start = end
    while (start > 0) {
      const r = this.records[start - 1]
      const n = outputBytes(r)
      if (start < end && bytes + n > budget) break
      bytes += n
      start--
    }
    return start
  }

  /**
   * The tail of records[0, end) that fits budget: its records, and what comes before them. Like the server's tail, it
   * starts at a character: the end of an escape sequence the earlier output left open counts for the modes and is cut,
   * and so are UTF-8 continuation bytes of a character that began before it.
   */
  tail(end: number, budget: number): { records: RecordingRecord[]; tail: RecordingTail } {
    const start = this.windowStart(end, budget)
    let sum = emptySummary()
    let skipped = 0
    for (let i = 0; i < start; i++) {
      const r = this.records[i]
      sum = apply(null, sum, r)
      skipped += outputBytes(r)
    }
    const records = this.records.slice(start, end)
    let modes: { mode: number; on: boolean }[] = []
    if (start > 0) {
      const t = this.trackerAt(start)
      for (let i = 0; i < records.length; i++) {
        const r = records[i]
        if (r.k !== 'o') continue
        const d = decode(r.d)
        let n = t.finish(d)
        if (!t.inSequence()) while (n < d.length && (d[n] & 0xc0) === 0x80) n++
        if (n > 0) {
          skipped += n
          records[i] = { ...r, d: b64bytes(d.subarray(n)) }
        }
        if (n < d.length) break
      }
      modes = t.current()
    }
    return {
      records,
      tail: {
        from: records[0]?.seq ?? 0,
        skipped_bytes: skipped,
        cols: start > 0 ? sum.cols : 0,
        rows: start > 0 ? sum.rows : 0,
        modes,
        gaps: sum.gaps,
        truncated: sum.truncated,
      },
    }
  }
}
