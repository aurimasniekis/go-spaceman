import { describe, expect, it } from 'vitest'
import { clearedBy, holdTarget, limitKindLabel, localTime, utcTime, waitReason, type UsageLimit } from './usageLimit'

const hold = (extra: Partial<UsageLimit>): UsageLimit => ({ scope: 'account', kind: 'five_hour', source: 'transcript', seen_at: '2026-11-14T10:00:00Z', ...extra })

describe('usage-limit wording', () => {
  it('names each limit kind, and an unknown one as a usage limit', () => {
    expect(limitKindLabel('five_hour')).toBe('5-hour limit')
    expect(limitKindLabel('seven_day_opus')).toBe('weekly Opus limit')
    expect(limitKindLabel('gateway_budget')).toBe('gateway budget')
    expect(limitKindLabel('unknown')).toBe('usage limit')
    expect(limitKindLabel('something_new')).toBe('usage limit')
  })

  it('shows times in the given zone with the zone named, the day only when not near, and UTC for the tooltip', () => {
    const now = Date.parse('2026-11-14T10:00:00Z')
    // The zone's short name comes from the runtime's ICU data: an abbreviation or a GMT offset.
    expect(localTime('2026-11-14T13:05:00Z', now, 'Europe/Vilnius')).toMatch(/^15:05 (EET|GMT\+2)$/)
    expect(localTime('2026-11-14T13:05:00Z', now, 'America/New_York')).toMatch(/^08:05 (EST|GMT-5)$/)
    expect(localTime('2026-11-20T13:05:00Z', now, 'UTC')).toBe('Fri 20 Nov, 13:05 UTC')
    expect(utcTime('2026-11-14T13:05:42Z')).toBe('2026-11-14 13:05 UTC')
    expect(localTime(undefined, now)).toBe('unknown')
    expect(utcTime('not a time')).toBe('unknown')
  })

  it('names what a hold holds: the bound account, or the gateway or its project budget', () => {
    const names = (id: string) => (id === 'acc_1' ? 'team-max' : undefined)
    expect(holdTarget(hold({ account_id: 'acc_1' }), names)).toBe('team-max')
    expect(holdTarget(hold({ account_id: 'acc_9' }), names)).toBe('acc_9')
    expect(holdTarget(hold({ scope: 'gateway', kind: 'gateway_quota', gateway_ref: 'harbor-pilot' }), names)).toBe('Harbor Pilot gateway')
    expect(holdTarget(hold({ scope: 'gateway', kind: 'gateway_budget', gateway_ref: 'harbor-pilot:project:prj_web' }), names)).toBe('Harbor Pilot project prj_web')
  })

  it('says why a wait waits and who cleared a hold, never naming another space', () => {
    expect(waitReason('node_offline')).toBe('the node is not connected')
    expect(waitReason('brand_new_reason')).toBe('brand new reason')
    expect(waitReason(undefined)).toBe('')
    expect(clearedBy('run:run_1')).toBe('a run reported it over')
    expect(clearedBy('evidence')).toBe("another space's run reported it over")
    expect(clearedBy('user:usr_1')).toBe('user:usr_1')
  })
})
