import type { RecordingRecord } from '@/api/client'
import { apply, decode, emptySummary, MAX_TAIL_BYTES, modePrelude, ModeTracker, nextTail, ReplayIndex, TAIL_BYTES, tailRecords } from './records'

const enc = new TextEncoder()
const out = (seq: number, s: string): RecordingRecord => ({ seq, t: seq * 1000, k: 'o', d: btoa(s) })
const text = (rs: RecordingRecord[]) => rs.map((r) => (r.k === 'o' ? new TextDecoder().decode(decode(r.d)) : `<${r.k}>`)).join('')

describe('tail sizes', () => {
  it('grows 1, 4, 16 MiB and then asks for everything', () => {
    expect(TAIL_BYTES).toBe(1 << 20)
    expect(nextTail(TAIL_BYTES)).toBe(4 << 20)
    expect(nextTail(4 << 20)).toBe(MAX_TAIL_BYTES)
    expect(nextTail(MAX_TAIL_BYTES)).toBe(0)
    expect(nextTail(0)).toBe(0)
  })
})

describe('tailRecords', () => {
  it('turns what comes before a tail into records with seq 0', () => {
    const rs = tailRecords({ from: 9, skipped_bytes: 100, cols: 80, rows: 24, modes: [{ mode: 1049, on: true }, { mode: 25, on: false }], gaps: 2, truncated: true })
    expect(rs.every((r) => r.seq === 0)).toBe(true)
    expect(text(rs)).toBe('<size>\x1b[?1049h\x1b[?25l<gap><gap><trunc>')
    const sum = rs.reduce((s, r) => apply(null, s, r), emptySummary())
    expect(sum).toMatchObject({ cols: 80, rows: 24, gaps: 2, truncated: true, lastSeq: 0 })
  })
  it('adds nothing for a tail that is the whole recording', () => {
    expect(tailRecords({ from: 1, skipped_bytes: 0, cols: 0, rows: 0, modes: [], gaps: 0, truncated: false })).toEqual([])
    expect(modePrelude([])).toBe('')
  })
})

describe('ModeTracker', () => {
  // The same cases as internal/recording TestModeTracker: the server and the replay must agree.
  it('follows private modes across split writes', () => {
    const m = new ModeTracker()
    for (const w of ['hi\x1b[?10', '49h\x1b[?25', 'l text \x1b[2J\x1b[?1000;1006;2026h', '\x1b[?1000l\x1b', '[?2004h']) m.write(enc.encode(w))
    expect(m.current()).toEqual([
      { mode: 1049, on: true },
      { mode: 25, on: false },
      { mode: 1006, on: true },
      { mode: 1000, on: false },
      { mode: 2004, on: true },
    ])
    m.write(enc.encode('\x1bc\x1b[?99999999999h\x1b[?1\x1b[?47h'))
    expect(m.current()).toEqual([{ mode: 47, on: true }])
    m.write(enc.encode('\x1b[?' + '1;'.repeat(40) + '25l'))
    expect(m.current()).toEqual([{ mode: 47, on: true }])
  })
})

describe('ReplayIndex', () => {
  const records: RecordingRecord[] = [{ seq: 1, t: 0, k: 'open', cols: 200, rows: 50 }]
  for (let i = 0; i < 1500; i++) {
    const c = i % 2 === 0 ? 'h' : 'l'
    records.push(out(records.length + 1, `\x1b[?1049${c}\x1b[?1000${c}line ${String(i).padStart(4, '0')}\r\n`))
    if (i === 700) records.push({ seq: records.length + 1, t: 0, k: 'size', cols: 120, rows: 40 })
    if (i === 800) records.push({ seq: records.length + 1, t: 0, k: 'gap', why: 'reattach' })
  }

  it('renders a seek from a bounded window after the state before it', () => {
    const idx = new ReplayIndex(records)
    let slices = 0
    while (!idx.advance(1400, 0)) slices++
    expect(slices).toBeGreaterThan(0) // indexed in slices
    const end = 1400
    // Each record is 27 bytes of output: ten fit in 270.
    const { records: window, tail } = idx.tail(end, 27 * 10)
    const start = end - window.length
    expect(window).toHaveLength(10)
    expect(tail.from).toBe(records[start].seq)
    expect(tail).toMatchObject({ cols: 120, rows: 40, gaps: 1, truncated: false })
    // The last record before start decides the modes.
    const lastBefore = text([records[start - 1]])
    const on = lastBefore.startsWith('\x1b[?1049h')
    expect(tail.modes).toEqual([
      { mode: 1049, on },
      { mode: 1000, on },
    ])
    expect(tail.skipped_bytes).toBe(records.slice(0, start).reduce((n, r) => n + (r.k === 'o' ? decode(r.d).length : 0), 0))
    // Unindexed records past the last checkpoint give the same modes.
    expect(new ReplayIndex(records).modesAt(start)).toEqual(tail.modes)
  })

  it('keeps at least one record and needs no prelude from the start', () => {
    const idx = new ReplayIndex(records)
    expect(idx.windowStart(5, 1)).toBe(4)
    const { records: window, tail } = idx.tail(5, 1 << 20)
    expect(window).toHaveLength(5)
    expect(tail).toMatchObject({ skipped_bytes: 0, cols: 0, modes: [] })
  })
})

describe('a tail starts at a character', () => {
  // The same cases as internal/recording TestTailStartsAtACharacter.
  const bytes = (s: string) => btoa(s)
  const raw = (seq: number, s: string): RecordingRecord => ({ seq, t: seq, k: 'o', d: bytes(s) })
  const tailOf = (parts: string[], budget: number) => {
    const records = parts.map((p, i) => raw(i + 1, p))
    const idx = new ReplayIndex(records)
    idx.advance()
    return idx.tail(records.length, budget)
  }
  const joined = (rs: RecordingRecord[]) => rs.map((r) => atob(r.d ?? '')).join('')

  it('counts a DECSET the cut falls inside and does not draw its end', () => {
    const { records, tail } = tailOf(['earlier output\x1b[?10', '49hfull screen app'], 20)
    expect(joined(records)).toBe('full screen app')
    expect(tail.modes).toEqual([{ mode: 1049, on: true }])
    expect(tail.skipped_bytes).toBe('earlier output\x1b[?10'.length + 3)
  })

  it.each([
    [['aaaa\x1b[38;2;1', '0;20;30mgreen'], 'green'],
    [['aaaa\x1b]0;a long ti', 'tle', ' still\x07after'], 'after'],
    [['aaaa\x1b]0;t\x1b', '\\after'], 'after'],
    [['aaaa\xe2\x94', '\x80\xe2\x94\x80 rule'], '\xe2\x94\x80 rule'],
    [['aaaa', 'plain'], 'plain'],
  ])('%j starts at %j', (parts, want) => {
    const budget = parts[parts.length - 1].length + parts[parts.length - 2].length - 1
    expect(joined(tailOf(parts, budget).records)).toBe(want)
  })
})
