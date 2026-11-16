import { describe, expect, it } from 'vitest'
import type { UsageRow } from '@/api/client'
import { bucketStarts, buildChart, compact, estimate, niceScale, OTHER } from './series'

const row = (bucket_start: string, group: string, total_tokens: number) => ({ bucket_start, group, total_tokens }) as UsageRow

describe('buildChart', () => {
  const days = ['2026-11-14T00:00:00Z', '2026-11-15T00:00:00Z', '2026-11-16T00:00:00Z']
  it('stacks groups per bucket in a fixed (name) order and keeps empty buckets', () => {
    const c = buildChart([row(days[0], 'worker', 10), row(days[0], 'control', 5), row(days[2], 'worker', 7)], days)
    expect(c.groups).toEqual(['control', 'worker'])
    expect(c.stacks.map((s) => s.total)).toEqual([15, 0, 7])
    expect(c.max).toBe(15)
  })
  it('folds groups beyond the seventh into Other, last', () => {
    const rows = Array.from({ length: 9 }, (_, i) => row(days[0], `g${i}`, 100 - i))
    const c = buildChart(rows, days)
    expect(c.groups).toHaveLength(8)
    expect(c.groups.at(-1)).toBe(OTHER)
    expect(c.stacks[0].segments.find((s) => s.group === OTHER)?.tokens).toBe(93 + 92)
  })
  it('labels an empty group and maps ids to names', () => {
    const c = buildChart([row(days[0], '', 1), row(days[0], 'acc_1', 2)], days, { acc_1: 'team-max' })
    expect(c.groups).toEqual(['None', 'team-max'])
  })
})

describe('bucketStarts', () => {
  it('lists whole UTC days and hours', () => {
    expect(bucketStarts('2026-11-14T10:00:00Z', '2026-11-16T00:00:00Z', 'day')).toEqual(['2026-11-14T00:00:00Z', '2026-11-15T00:00:00Z'])
    expect(bucketStarts('2026-11-14T10:30:00Z', '2026-11-14T12:00:00Z', 'hour')).toEqual(['2026-11-14T10:00:00Z', '2026-11-14T11:00:00Z'])
  })
})

describe('niceScale and formats', () => {
  it('rounds the axis up to 1, 2 or 5 steps', () => {
    expect(niceScale(0).top).toBe(1)
    expect(niceScale(930).ticks).toEqual([0, 500, 1000])
    expect(niceScale(7200).top).toBe(8000)
  })
  it('shortens tokens and labels cost as an estimate', () => {
    expect([compact(950), compact(1200), compact(15_400), compact(3_400_000)]).toEqual(['950', '1.2K', '15K', '3.4M'])
    expect(estimate(0.0123)).toBe('≈ $0.012')
    expect(estimate(12.5)).toBe('≈ $12.50')
  })
})
