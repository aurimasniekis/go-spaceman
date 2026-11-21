import { describe, expect, it } from 'vitest'
import type { UsageRow } from '@/api/client'
import { notCountedLines, parseUsageSearch, perGroup, resolveUsage, serializeUsage, toggleFilter } from './state'

describe('usage URL state', () => {
  it('keeps valid values and drops unknown ones', () => {
    expect(parseUsageSearch({ range: '90d', group: 'model', space: 'spc_1, spc_2,spc_1', account: '', harness: 'codex', junk: 1 })).toEqual({ range: '90d', group: 'model', space: 'spc_1,spc_2', harness: 'codex' })
    expect(parseUsageSearch({ range: '1y', group: 'user', space: 7, model: ['a'] })).toEqual({})
  })
  it('caps a filter at 50 values', () => {
    const many = Array.from({ length: 80 }, (_, i) => `m${i}`).join(',')
    expect(parseUsageSearch({ model: many }).model?.split(',')).toHaveLength(50)
  })
  it('resolves defaults and round-trips without writing them out', () => {
    const st = resolveUsage({})
    expect(st).toEqual({ range: '7d', group: 'space', space: [], account: [], model: [], harness: [] })
    expect(serializeUsage(st)).toEqual({})
    const s = { range: '90d' as const, group: 'account' as const, space: 'spc_1', model: 'opus,sonnet' }
    expect(serializeUsage(resolveUsage(s))).toEqual(s)
  })
  it('falls back to the default group when the scope does not allow the one in the URL', () => {
    expect(resolveUsage({ group: 'space' }, 'role', ['role', 'account']).group).toBe('role')
  })
  it('toggles a filter on and off', () => {
    const on = toggleFilter(resolveUsage({}), 'account', 'acc_1')
    expect(on.account).toEqual(['acc_1'])
    expect(toggleFilter(on, 'account', 'acc_1').account).toEqual([])
  })
})

const row = (group: string, tokens: number): UsageRow => ({ group, total_tokens: tokens, input_tokens: tokens, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, requests: 1, total_cost_usd: 0.5 }) as UsageRow

describe('usage data shaping', () => {
  it('sums rows per group, biggest first', () => {
    const out = perGroup([row('a', 10), row('b', 30), row('a', 25)])
    expect(out.map((r) => [r.group, r.total_tokens, r.requests, r.total_cost_usd])).toEqual([['a', 35, 2, 1], ['b', 30, 1, 0.5]])
  })
  it('lists only harnesses with runs, naming Harbor Pilot read-backs', () => {
    expect(notCountedLines([{ harness: 'claude-code', runs: 0, harbor_pilot_runs: 0 }, { harness: 'codex', runs: 4, harbor_pilot_runs: 2 }, { harness: 'other', runs: 1, harbor_pilot_runs: 0 }]))
      .toEqual(['4 Codex runs (2 read back from Harbor Pilot)', '1 other run'])
    expect(notCountedLines(undefined)).toEqual([])
  })
})
