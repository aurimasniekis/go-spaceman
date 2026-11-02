import type { RecordingRecord } from '@/api/client'
import { apply, decode, emptySummary, indexAt, offsetMs, renderTo, type Screen } from './records'

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
    const sum = renderTo(s, recs, recs.length)
    expect(s.out).toBe('hello world')
    expect(s.size).toBe('80x24')
    expect(sum).toMatchObject({ gaps: 1, closed: true, exitCode: 3, reason: 'exited', lastSeq: 6, cols: 80, rows: 24 })
  })

  it('seeks by re-rendering from the start', () => {
    const s = new FakeScreen()
    renderTo(s, recs, 3)
    expect(s.out).toBe('hello ')
    expect(s.size).toBe('200x120')
    renderTo(s, recs, 5)
    expect(s.out).toBe('hello world')
    expect(s.resets).toBe(2)
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
