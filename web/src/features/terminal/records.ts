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
