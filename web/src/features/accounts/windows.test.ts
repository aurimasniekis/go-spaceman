import { describe, expect, it } from 'vitest'
import { pct, windowStale } from './windows'

const now = Date.parse('2026-11-16T12:00:00Z')
describe('windowStale', () => {
  it('is fresh within the hour and before the reset', () => {
    expect(windowStale({ observed_at: '2026-11-16T11:30:00Z', resets_at: '2026-11-16T15:00:00Z' }, now)).toBe(false)
  })
  it('is stale after an hour without a report', () => {
    expect(windowStale({ observed_at: '2026-11-16T10:59:00Z', resets_at: '2026-11-16T15:00:00Z' }, now)).toBe(true)
  })
  it('is stale once its reset has passed', () => {
    expect(windowStale({ observed_at: '2026-11-16T11:59:00Z', resets_at: '2026-11-16T11:00:00Z' }, now)).toBe(true)
  })
})
describe('pct', () => {
  it('rounds and clamps', () => {
    expect([pct(-3), pct(42.4), pct(42.5), pct(130)]).toEqual([0, 42, 43, 100])
  })
})
