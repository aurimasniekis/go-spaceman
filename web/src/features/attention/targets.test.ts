import { describe, expect, it } from 'vitest'
import { attentionSummary, groupAttention, summaryText, kindOptions, spaceOptions, targetRoute, truncate, truncatedKinds, type AttentionItem, type AttentionSpace } from './targets'

const item = (p: Partial<AttentionItem>): AttentionItem => ({
  id: 'x', kind: 'task_blocked', severity: 'act', since: '2026-11-20T10:00:00Z', space_id: 'spc_a', title: 't',
  target: { type: 'task', id: 'tsk_1', space_id: 'spc_a' }, ...p,
}) as AttentionItem
const space = (p: Partial<AttentionSpace>): AttentionSpace => ({ space_id: 'spc_a', name: 'Alpha', slug: 'a', state: 'active', total: 0, ...p }) as AttentionSpace

describe('targetRoute', () => {
  it('maps each target type to a route', () => {
    expect(targetRoute({ type: 'run', id: 'run_1', space_id: 's' })).toEqual({ to: '/spaces/$spaceId/runs/$runId', params: { spaceId: 's', runId: 'run_1' } })
    expect(targetRoute({ type: 'task', id: 'tsk_1', space_id: 's' })).toEqual({ to: '/spaces/$spaceId/queue', params: { spaceId: 's' }, search: { task: 'tsk_1' } })
    expect(targetRoute({ type: 'subscription', id: 'sub_1', space_id: 's' })).toEqual({ to: '/spaces/$spaceId/events/$subscriptionId', params: { spaceId: 's', subscriptionId: 'sub_1' } })
    expect(targetRoute({ type: 'space_usage', space_id: 's' })?.to).toBe('/spaces/$spaceId/usage')
    expect(targetRoute({ type: 'space_budget', space_id: 's' })?.to).toBe('/spaces/$spaceId/limits')
    expect(targetRoute({ type: 'xspace_request', id: 'xr', space_id: 's' })?.to).toBe('/spaces/$spaceId/cross-space')
  })
  it('falls back to the space page for a run without an ID', () => {
    expect(targetRoute({ type: 'run', space_id: 's' })).toEqual({ to: '/spaces/$spaceId', params: { spaceId: 's' } })
  })
  it('leaves the web-only pages unlinked in the Mini App', () => {
    for (const type of ['subscription', 'space_usage', 'space_budget', 'xspace_request'] as const) expect(targetRoute({ type, id: 'i', space_id: 's' }, true)).toBeNull()
    expect(targetRoute({ type: 'run', id: 'r', space_id: 's' }, true)).not.toBeNull()
    expect(targetRoute({ type: 'task', id: 't', space_id: 's' }, true)).not.toBeNull()
  })
})

describe('groupAttention', () => {
  const items = [
    item({ id: '1', since: '2026-11-20T08:00:00Z' }),
    item({ id: '2', kind: 'run_parked', severity: 'watch', space_id: 'spc_b', since: '2026-11-20T09:00:00Z' }),
    item({ id: '3', kind: 'xspace_request', since: '2026-11-20T11:00:00Z' }),
  ]
  const spaces = [space({ human_pending: 2, human_overdue: 1 }), space({ space_id: 'spc_c', name: 'Gamma', human_pending: 1 })]
  it('groups by severity, newest first, with one collapsed human row first', () => {
    const g = groupAttention({ items, spaces })
    expect(g.act.map((r) => r.key)).toEqual(['human:all', 'xspace_request:3', 'task_blocked:1'])
    expect(g.watch.map((r) => r.key)).toEqual(['run_parked:2'])
    expect(g.act[0]).toMatchObject({ type: 'human', count: 3, overdue: 1 })
  })
  it('filters by space and kind, and the human row follows them', () => {
    expect(groupAttention({ items, spaces }, { space: 'spc_b' }).act).toEqual([])
    expect(groupAttention({ items, spaces }, { space: 'spc_a' }).act[0]).toMatchObject({ type: 'human', count: 2, spaceId: 'spc_a' })
    expect(groupAttention({ items, spaces }, { kind: 'run_parked' })).toMatchObject({ act: [], watch: [{ key: 'run_parked:2' }] })
    expect(groupAttention({ items, spaces }, { kind: 'human' }).act.map((r) => r.type)).toEqual(['human'])
  })
  it('lists a space that spaces[] leaves out', () => {
    const g = groupAttention({ items, spaces: [] })
    expect(g.watch).toHaveLength(1)
    expect(spaceOptions({ items, spaces: [] }).map((o) => o.value)).toEqual(['spc_a', 'spc_b'])
  })
})

describe('options and truncation', () => {
  it('names spaces and kinds', () => {
    expect(spaceOptions({ items: [item({ space_id: 'spc_z' })], spaces: [space({})] })).toEqual([{ value: 'spc_a', label: 'Alpha' }, { value: 'spc_z', label: 'spc_z' }])
    expect(kindOptions({ items: [item({})], spaces: [space({ human_pending: 1 })] }).map((o) => o.value)).toEqual(['task_blocked', 'human'])
  })
  it('reports kinds cut off by the cap', () => {
    expect(truncatedKinds({ items: [item({})], item_counts: { task_blocked: 25, quarantine: 0 } })).toEqual([{ kind: 'task_blocked', shown: 1, total: 25 }])
  })
  it('shortens long agent titles', () => {
    expect(truncate('a  b\n c')).toBe('a b c')
    expect(truncate('x'.repeat(200)).length).toBe(120)
  })
})

describe('summaryText', () => {
  const rows = [
    item({ id: '1', kind: 'account_switch_failed' }),
    item({ id: '2', kind: 'xspace_request' }),
    item({ id: '3', kind: 'run_parked', severity: 'watch' }),
  ]
  it('counts the listed Act now rows, including ones the server total leaves out, and the watch rows', () => {
    const spaces = [space({ total: 0 })]
    expect(attentionSummary({ items: rows, spaces })).toEqual({ act: 2, watch: 1 })
    expect(summaryText({ items: rows, spaces })).toBe('2 items need a person · 1 to watch')
  })
  it('counts uncapped kinds from item_counts', () => {
    expect(summaryText({ items: [rows[0]], spaces: [space({})], item_counts: { account_switch_failed: 25 } })).toBe('25 items need a person')
  })
  it('falls back to the total when only non-row categories are counted', () => {
    expect(summaryText({ items: [], spaces: [space({ total: 3 })], total: 3 })).toBe('3 items need attention')
    expect(summaryText({ items: [], spaces: [space({})], total: 0 })).toBe('')
  })
  it('adds human actions to Act now and never says 0 above rows', () => {
    expect(summaryText({ items: [], spaces: [space({ human_pending: 1 })] })).toBe('1 item needs a person')
    expect(summaryText({ items: [rows[2]], spaces: [space({})] })).toBe('1 to watch')
  })
})
