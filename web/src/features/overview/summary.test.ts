import { describe, expect, it } from 'vitest'
import type { UsageRow } from '@/api/client'
import { delta, overviewRange, summarize } from './summary'

const NOW = Date.UTC(2026, 10, 20, 15, 0, 0)
const { from, to } = overviewRange(NOW)
const row = (dayIdx: number, group: string, tokens: number) =>
  ({ bucket_start: new Date(Date.parse(from) + dayIdx * 86_400_000).toISOString().replace('.000Z', 'Z'), group, total_tokens: tokens }) as UsageRow

describe('overviewRange', () => {
  it('is 14 whole UTC days ending after today', () => {
    expect(from).toBe('2026-11-07T00:00:00Z')
    expect(to).toBe('2026-11-21T00:00:00Z')
  })
})

describe('summarize', () => {
  it('splits today, the last 7 days and the 7 before, and ranks the top groups', () => {
    const rows = [row(13, 'a', 100), row(12, 'b', 50), row(7, 'a', 10), row(6, 'a', 500), row(0, 'c', 5), row(14, 'x', 9)]
    const s = summarize(rows, from)
    expect(s.today).toBe(100)
    expect(s.week).toBe(160)
    expect(s.previous).toBe(505)
    expect(s.days).toHaveLength(14)
    expect(s.top).toEqual([{ group: 'a', tokens: 110 }, { group: 'b', tokens: 50 }])
  })
  it('keeps at most five top groups', () => {
    const rows = ['a', 'b', 'c', 'd', 'e', 'f', 'g'].map((g, i) => row(13, g, 10 + i))
    expect(summarize(rows, from).top.map((t) => t.group)).toEqual(['g', 'f', 'e', 'd', 'c'])
  })
})

describe('delta', () => {
  it('compares with the previous period', () => {
    expect(delta(150, 100)).toEqual({ pct: 50, up: true })
    expect(delta(50, 100)).toEqual({ pct: 50, up: false })
  })
  it('has nothing to compare without a previous period', () => {
    expect(delta(10, 0)).toBeUndefined()
  })
})
