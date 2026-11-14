import { describe, expect, it } from 'vitest'
import { browserZone, knownZones, zoneOptions } from './timezone'

describe('time zone picker (ADR-0083)', () => {
  it('lists known zones with the stored and browser zones added once, sorted', () => {
    expect(zoneOptions(['UTC', 'Europe/Vilnius'], 'Asia/Kolkata', 'Europe/Vilnius')).toEqual(['Asia/Kolkata', 'Europe/Vilnius', 'UTC'])
    expect(zoneOptions(['UTC'], '', '')).toEqual(['UTC'])
  })
  it('always offers UTC and reads the browser zone', () => {
    expect(knownZones()).toContain('UTC')
    expect(typeof browserZone()).toBe('string')
  })
})
