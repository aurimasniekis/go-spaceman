import { describe, expect, it } from 'vitest'
import type { AttentionItem } from '@/api/client'
import { itemLink, topItems } from './targets'

const item = (over: Partial<AttentionItem> & { type?: AttentionItem['target']['type']; tid?: string }): AttentionItem => ({
  id: over.id ?? 'i', kind: 'task_failed', severity: over.severity ?? 'act', since: over.since ?? '2026-11-20T10:00:00Z', space_id: 'spc_1', title: 't',
  target: { type: over.type ?? 'task', space_id: 'spc_1', id: over.tid },
})

describe('itemLink', () => {
  it('maps targets to routes', () => {
    expect(itemLink(item({ type: 'run', tid: 'run_1' }))).toEqual({ to: '/spaces/$spaceId/runs/$runId', params: { spaceId: 'spc_1', runId: 'run_1' } })
    expect(itemLink(item({ type: 'task' })).to).toBe('/spaces/$spaceId/queue')
    expect(itemLink(item({ type: 'subscription', tid: 'sub_1' })).to).toBe('/spaces/$spaceId/events/$subscriptionId')
    expect(itemLink(item({ type: 'space_budget' })).to).toBe('/spaces/$spaceId/limits')
    expect(itemLink(item({ type: 'space_usage' })).to).toBe('/spaces/$spaceId/usage')
    expect(itemLink(item({ type: 'xspace_request' })).to).toBe('/spaces/$spaceId/cross-space')
  })
  it('falls back to the space when the id is missing', () => {
    expect(itemLink(item({ type: 'run' })).to).toBe('/spaces/$spaceId')
  })
})

describe('topItems', () => {
  it('puts act before watch, newest first, at most five', () => {
    const items = [
      item({ id: 'w1', severity: 'watch', since: '2026-11-20T12:00:00Z' }), item({ id: 'a1', since: '2026-11-20T08:00:00Z' }), item({ id: 'a2', since: '2026-11-20T09:00:00Z' }),
      item({ id: 'a3', since: '2026-11-20T07:00:00Z' }), item({ id: 'a4', since: '2026-11-20T06:00:00Z' }), item({ id: 'a5', since: '2026-11-20T05:00:00Z' }),
    ]
    expect(topItems(items).map((i) => i.id)).toEqual(['a2', 'a1', 'a3', 'a4', 'a5'])
  })
})
