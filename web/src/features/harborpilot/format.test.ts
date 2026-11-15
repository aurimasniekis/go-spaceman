import { describe, expect, it } from 'vitest'
import { budgetBadge, durationLabel, gatewayAddressChanged, gatewayVersionLabel, harnessLabel, periodLabel, resetLabel, ttlError, windowLabel } from './format'

describe('Harbor Pilot settings format', () => {
  it('labels token lifetimes', () => {
    expect(durationLabel(86400)).toBe('24 h')
    expect(durationLabel(604800)).toBe('7 days')
    expect(durationLabel(900)).toBe('15 min')
    expect(durationLabel(5400)).toBe('1 h 30 min')
    expect(durationLabel(7200)).toBe('2 h')
    expect(durationLabel(0)).toBe('—')
  })
  it('checks the lifetime bounds (15 min to 7 days)', () => {
    expect(ttlError(899)).toBeDefined()
    expect(ttlError(900)).toBeUndefined()
    expect(ttlError(604800)).toBeUndefined()
    expect(ttlError(604801)).toBeDefined()
  })
  it('names harnesses, keeping unknown kinds', () => {
    expect(harnessLabel('claude-code')).toBe('Claude Code')
    expect(harnessLabel('gemini-cli')).toBe('gemini-cli')
  })
  it('tells when a connection edit moves the gateway (the tokens are then needed again)', () => {
    const cur = { base_url: 'https://hp.example.com', run_base_url: '', allow_insecure_http: false }
    expect(gatewayAddressChanged(null, 'https://hp.example.com', '', false)).toBe(false)
    expect(gatewayAddressChanged(cur, 'https://hp.example.com/', '', false)).toBe(false)
    expect(gatewayAddressChanged(cur, 'https://hp.example.com', 'https://hp.example.com', false)).toBe(false)
    expect(gatewayAddressChanged(cur, 'https://elsewhere.example.com', '', false)).toBe(true)
    expect(gatewayAddressChanged(cur, 'https://hp.example.com/gw', '', false)).toBe(true)
    expect(gatewayAddressChanged(cur, 'https://hp.example.com', 'https://runs.example.com', false)).toBe(true)
    expect(gatewayAddressChanged(cur, 'https://hp.example.com', '', true)).toBe(true)
    expect(gatewayAddressChanged({ ...cur, allow_insecure_http: true }, 'https://hp.example.com', '', false)).toBe(false)
  })

  it('labels Harbor Pilot 0.1.4 budget states, periods and ends', () => {
    expect(budgetBadge('scheduled', 'block').label).toBe('Scheduled')
    expect(budgetBadge('expired', 'block').label).toBe('Ended')
    expect(budgetBadge('paused', 'block').label).toBe('Unknown state')
    expect(periodLabel('once')).toBe('one time')
    expect(periodLabel('day')).toBe('per day')
    expect(resetLabel({ period: 'once', state: 'ok' })).toBe('Never')
    expect(resetLabel({ period: 'once', state: 'ok', period_end: '2027-01-01T00:00:00Z' })).toMatch(/^Never; ends /)
    expect(resetLabel({ period: 'day', state: 'ok' })).toBe('—')
    expect(windowLabel('once')).toBe('since the one-time budget started counting')
  })

  it('labels the gateway version by where it came from', () => {
    expect(gatewayVersionLabel({ source: 'system', version: '0.1.4' })).toBe('0.1.4')
    expect(gatewayVersionLabel({ source: 'legacy' })).toBe('older than 0.1.4')
    expect(gatewayVersionLabel({ source: 'unknown' })).toBe('unknown')
  })
})
