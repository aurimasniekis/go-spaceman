import type { RecordingRecord } from '@/api/client'
import { apply, decode, emptySummary, feed, indexAt, offsetMs, type RecordingSummary, type Screen } from './records'

class FakeScreen implements Screen {
  out = ''
  size = ''
  resets = 0
  write(d: string | Uint8Array) {
    this.out += typeof d === 'string' ? d : new TextDecoder().decode(d)
  }
  resize(c: number, r: number) {
    this.size = `${c}x${r}`
  }
  reset() {
    this.out = ''
    this.resets++
  }
}

const b64 = (s: string) => btoa(s)
/** Applies records in order from a clean summary, the way a seek's feed does. */
const applyAll = (s: Screen, rs: RecordingRecord[]): RecordingSummary => rs.reduce((sum, r) => apply(s, sum, r), emptySummary())
const recs: RecordingRecord[] = [
  { seq: 1, t: 1_000_000, k: 'open', cols: 200, rows: 120, run: 'run_x' },
  { seq: 2, t: 1_500_000, k: 'o', d: b64('hello ') },
  { seq: 3, t: 2_000_000, k: 'gap', why: 'reattach' },
  { seq: 4, t: 2_500_000, k: 'size', cols: 80, rows: 24 },
  { seq: 5, t: 3_000_000, k: 'o', d: b64('world') },
  { seq: 6, t: 4_000_000, k: 'close', exit_code: 3, reason: 'exited' },
]

describe('recording records', () => {
  it('decodes base64 output bytes', () => {
    // Output is raw bytes: a UTF-8 sequence split across records must survive untouched.
    expect(Array.from(decode('4pyT'))).toEqual([0xe2, 0x9c, 0x93])
    expect(decode(undefined).length).toBe(0)
  })

  it('renders output, sizes, gaps and the close', () => {
    const s = new FakeScreen()
    const sum = applyAll(s, recs)
    expect(s.out).toBe('hello world')
    expect(s.size).toBe('80x24')
    expect(sum).toMatchObject({ gaps: 1, closed: true, exitCode: 3, reason: 'exited', lastSeq: 6, cols: 80, rows: 24 })
  })

  it('marks truncation without drawing it', () => {
    const s = new FakeScreen()
    const sum = apply(s, emptySummary(), { seq: 9, t: 0, k: 'trunc', why: 'quota' })
    expect(sum.truncated).toBe(true)
    expect(s.out).toBe('')
  })

  it('maps between offsets and indexes', () => {
    expect(offsetMs(recs, 0)).toBe(0)
    expect(offsetMs(recs, 5)).toBe(3000)
    expect(indexAt(recs, 0)).toBe(0)
    expect(indexAt(recs, 1500)).toBe(3)
    expect(indexAt(recs, 99_000)).toBe(recs.length)
  })
})

describe('feed', () => {
  /** A clock that advances `cost` ms per write, and a scheduler the test runs by hand. */
  function harness(cost: number) {
    let clock = 0
    const tasks: (() => void)[] = []
    const screen = new FakeScreen()
    const writes: number[] = []
    const write = screen.write.bind(screen)
    screen.write = (d) => {
      writes.push(typeof d === 'string' ? d.length : d.length)
      clock += cost
      write(d)
    }
    const slices: { lastSeq: number; backlog: number }[] = []
    const f = feed(screen, emptySummary(), (s, backlog) => slices.push({ lastSeq: s.lastSeq, backlog }), {
      now: () => clock,
      schedule: (run) => void tasks.push(run),
      sliceMs: 10,
      maxWriteBytes: 8,
    })
    const runAll = () => {
      let n = 0
      while (tasks.length) {
        tasks.shift()!()
        n++
      }
      return n
    }
    return { f, screen, writes, slices, tasks, runAll }
  }

  it('applies records in later tasks, in slices, with the same result as applying them in order', () => {
    const h = harness(6)
    h.f.push(recs)
    expect(h.screen.out).toBe('') // never in the caller's task
    expect(h.f.backlog()).toBe(6)
    const tasks = h.runAll()
    expect(h.screen.out).toBe('hello world')
    expect(h.screen.size).toBe('80x24')
    expect(tasks).toBeGreaterThan(1) // writes of 6 ms each in slices of 10 ms
    expect(h.slices.at(-1)).toEqual({ lastSeq: 6, backlog: 0 })
    expect(h.slices.slice(0, -1).every((s) => s.backlog > 0)).toBe(true)
    expect(h.f.backlog()).toBe(0)
  })

  it('writes consecutive output together, up to the write size', () => {
    const h = harness(0)
    h.f.push(['abc', 'def', 'ghi', 'jk'].map((d, i) => ({ seq: i + 1, t: i, k: 'o', d: b64(d) }) as RecordingRecord))
    h.runAll()
    expect(h.screen.out).toBe('abcdefghijk')
    expect(h.writes).toEqual([9, 2]) // 3 + 3 + 3 reaches the 8-byte bound
    expect(h.slices.at(-1)?.lastSeq).toBe(4)
  })

  it('applies nothing after stop', () => {
    const h = harness(4)
    h.f.push(recs)
    h.tasks.shift()!()
    const out = h.screen.out
    h.f.stop()
    h.f.push(recs)
    h.runAll()
    expect(h.screen.out).toBe(out)
    expect(h.f.backlog()).toBe(0)
  })
})
