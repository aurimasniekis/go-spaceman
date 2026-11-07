import { describe, expect, it } from 'vitest'
import { pendingSchedule } from './scheduled'

describe('pendingSchedule', () => {
  const now = Date.parse('2026-11-07T12:00:00Z')
  it('is pending only while stored and in the future', () => {
    expect(pendingSchedule({ state: 'stored', deliver_at: '2026-11-07T18:00:00Z' }, now)).toBe(true)
    expect(pendingSchedule({ state: 'stored', deliver_at: '2026-11-07T11:00:00Z' }, now)).toBe(false)
    expect(pendingSchedule({ state: 'resolved', deliver_at: '2026-11-07T18:00:00Z' }, now)).toBe(false)
    expect(pendingSchedule({ state: 'stored' }, now)).toBe(false)
    expect(pendingSchedule({ state: 'stored', deliver_at: 'nonsense' }, now)).toBe(false)
  })
})
