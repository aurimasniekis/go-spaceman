import { describe, expect, it } from 'vitest'
import { durationLabel, harnessLabel, ttlError } from './format'

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
})
