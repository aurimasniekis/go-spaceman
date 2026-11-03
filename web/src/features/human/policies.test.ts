import { minutes, policySummary } from './PoliciesPanel'

const base = {
  remind_after_minutes: 240, remind_every_minutes: 1440, max_reminders: 3, escalate_after_minutes: 1440, escalate_priority: 'high' as const,
  escalate_assignee: '', requester_ended: 'reroute' as const, hold_minutes: 60, successor_role: '', wake_idle: true, rate_per_run_hour: 30, rate_per_space_hour: 300,
}

describe('policy summaries', () => {
  it('formats minutes', () => {
    expect(minutes(0)).toBe('—')
    expect(minutes(90)).toBe('1h 30m')
    expect(minutes(1440)).toBe('1d')
    expect(minutes(1500)).toBe('1d 1h')
  })
  it('describes each concern', () => {
    const s = policySummary(base)
    expect(s.remind).toBe('after 4h, then every 1d (3×)')
    expect(s.escalate).toBe('after 1d: priority high')
    expect(s.ended).toBe('to control')
    expect(s.rate).toBe('30/run · 300/space per hour')
    expect(s.inbox).toBe('')
    expect(policySummary({ ...base, response_inbox: 'routed' }).inbox).toBe('routed responses become inbox items')
    expect(policySummary({ ...base, response_inbox: 'always' }).inbox).toBe('every response becomes an inbox item')
    const n = policySummary({ ...base, remind_after_minutes: 0, escalate_after_minutes: 0, requester_ended: 'next_launch', successor_role: 'reviewer' })
    expect(n.remind).toBe('none')
    expect(n.escalate).toBe('never')
    expect(n.ended).toBe('wait 1h for its next run, then reviewer')
  })
})
