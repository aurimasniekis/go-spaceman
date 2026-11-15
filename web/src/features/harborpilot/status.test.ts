import { describe, expect, it } from 'vitest'
import { usageBadge } from '@/features/usage/UsageCards'
import { budgetBadge, costLabel, scopeLabel, usedPercent, windowLabel } from './format'

describe('Harbor Pilot status format', () => {
  it('labels budget states; a used-up warn-only budget only warns', () => {
    expect(budgetBadge('exceeded', 'block').label).toBe('Used up')
    expect(budgetBadge('exceeded', 'warn')).toEqual({ label: 'Over (warn only)', color: 'owlYellow' })
    expect(budgetBadge('warning', 'block').label).toBe('Warning')
    expect(budgetBadge('ok', 'warn').label).toBe('OK')
    expect(budgetBadge('disabled', 'block').label).toBe('Disabled')
  })
  it('labels scopes, windows, costs and progress', () => {
    expect(scopeLabel('binding', 'cbn_1')).toBe('Client binding cbn_1')
    expect(scopeLabel('global', '')).toBe('Every request')
    expect(windowLabel('last_30_days')).toBe('the last 30 days')
    expect(windowLabel('month')).toBe('this month')
    expect(costLabel({ kind: 'actual_api_charge', currency: 'USD', amount: '1.5' })).toBe('1.5 USD charged')
    expect(usedPercent(1.7)).toBe(100)
    expect(usedPercent(0.256)).toBe(26)
  })
  it('says where a run usage comes from', () => {
    expect(usageBadge(true, true, 'harbor-pilot')).toBe('from Harbor Pilot')
    expect(usageBadge(true, false, 'claude-transcript')).toBe('from the transcript, so far')
    expect(usageBadge(false, true, 'unavailable')).toBe('unknown')
  })
})
