import { describe, expect, it } from 'vitest'
import type { HumanAction } from '@/api/client'
import { announcements, markOwnChange } from './notify'

const prefs = { enabled: true, desktop: false, new_actions: true, assigned_to_me: true, escalations: true, reminders: false, clarifications: true,
  min_priority: 'normal' as const, muted_spaces: [] as string[] }
const since = Date.parse('2026-11-03T10:00:00Z')
const base = { id: 'hum_1', space_id: 'spc_a', title: 'Rotate the key', status: 'open', priority: 'normal', requester_kind: 'agent',
  updated_at: '2026-11-03T09:00:00Z', comments: [] } as unknown as HumanAction
const run = (a: Partial<HumanAction>, o: Partial<{ created: boolean; me: string; seen: Set<string>; p: typeof prefs }> = {}) =>
  announcements({ ...base, ...a } as HumanAction, { prefs: o.p ?? prefs, me: o.me ?? 'usr_me', created: o.created ?? false, since, seen: o.seen ?? new Set(), space: 'A' })
    .map((n) => n.title)

describe('announcements (SM-HUI-006)', () => {
  it('announces new actions at or above the lowest priority, never in muted spaces', () => {
    expect(run({}, { created: true })).toEqual(['New human action'])
    expect(run({ priority: 'low' }, { created: true })).toEqual([])
    expect(run({}, { created: true, p: { ...prefs, muted_spaces: ['spc_a'] } })).toEqual([])
    expect(run({}, { created: true, p: { ...prefs, enabled: false } })).toEqual([])
    expect(run({ requester_kind: 'operator', requester_id: 'usr_me' } as Partial<HumanAction>, { created: true })).toEqual([]) // my own request
  })
  it('announces assignments to me whatever the priority, once', () => {
    const seen = new Set(['hum_1:assigned:usr_me'])
    expect(run({ assignee: 'usr_me', priority: 'low', updated_at: '2026-11-03T10:05:00Z' })).toEqual(['Assigned to you'])
    expect(run({ assignee: 'usr_me', updated_at: '2026-11-03T10:05:00Z' }, { seen })).toEqual([])
    expect(run({ assignee: 'usr_me', updated_at: '2026-11-03T09:05:00Z' })).toEqual([]) // before this page listened
    markOwnChange('hum_1') // claimed in this tab
    expect(run({ assignee: 'usr_me', updated_at: '2026-11-03T10:05:00Z' })).toEqual([])
  })
  it('announces escalations, and reminders only when chosen', () => {
    expect(run({ escalated_at: '2026-11-03T10:01:00Z', priority: 'urgent' })).toEqual(['Escalated to urgent'])
    expect(run({ reminded_at: '2026-11-03T10:01:00Z', reminder_count: 1 })).toEqual([])
    expect(run({ reminded_at: '2026-11-03T10:01:00Z', reminder_count: 1 }, { p: { ...prefs, reminders: true } })).toEqual(['Reminder: still waiting'])
  })
  it('announces a requester reply to my clarification request, and a clarification asked of me', () => {
    const comments = [{ id: 'c1', kind: 'clarification', author: 'usr_me', body: 'which?', created_at: '2026-11-03T10:01:00Z' },
      { id: 'c2', kind: 'reply', author: 'run_1', body: 'billing', created_at: '2026-11-03T10:02:00Z' }]
    expect(run({ comments } as Partial<HumanAction>)).toEqual(['The requester replied'])
    const asked = [{ id: 'c3', kind: 'clarification', author: 'usr_other', body: 'which host?', created_at: '2026-11-03T10:02:00Z' }]
    expect(run({ comments: asked, requester_kind: 'operator', requester_id: 'usr_me' } as Partial<HumanAction>)).toEqual(['More information requested'])
    expect(run({ comments: asked, requester_kind: 'operator', requester_id: 'usr_me' } as Partial<HumanAction>, { p: { ...prefs, clarifications: false } })).toEqual([])
  })
})
