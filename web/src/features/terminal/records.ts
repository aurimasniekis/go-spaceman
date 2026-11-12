import type { RecordingRecord } from '@/api/client'

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

/** Applies records[0..end) from a clean screen (seeking re-renders from the start: terminal state is not invertible). */
export function renderTo(screen: Screen, records: RecordingRecord[], end: number): RecordingSummary {
  screen.reset()
  let sum = emptySummary()
  for (let i = 0; i < Math.min(end, records.length); i++) sum = apply(screen, sum, records[i])
  return sum
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
