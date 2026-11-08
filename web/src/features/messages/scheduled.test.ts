import { describe, expect, it } from 'vitest'
import { messagesInView, pendingSchedule } from './scheduled'

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

describe('messagesInView', () => {
  const m = (id: string, over: Partial<Parameters<typeof messagesInView>[1][number]> = {}) =>
    ({ id, from_kind: 'run', kind: 'message', to_kind: 'control', state: 'available', ...over }) as const
  const recent = [m('plain'), m('note', { from_kind: 'system', kind: 'notification' }), m('ops', { to_kind: 'operators' })]
  it('shows the server scheduled list, not a filter of the recent messages', () => {
    // A reminder set long ago is not among the recent messages, but the scheduled list has it.
    const old = m('old-reminder', { state: 'stored', deliver_at: '2026-12-01T00:00:00Z' })
    expect(messagesInView('scheduled', recent, [old], false).map((x) => x.id)).toEqual(['old-reminder'])
    expect(messagesInView('scheduled', [...recent, old], [], false)).toEqual([])
  })
  it('splits the recent messages into people and system', () => {
    expect(messagesInView('people', recent, [], false).map((x) => x.id)).toEqual(['plain', 'ops'])
    expect(messagesInView('system', recent, [], false).map((x) => x.id)).toEqual(['note'])
    expect(messagesInView('all', recent, [], false)).toHaveLength(3)
    expect(messagesInView('all', recent, [], true).map((x) => x.id)).toEqual(['ops'])
  })
})
