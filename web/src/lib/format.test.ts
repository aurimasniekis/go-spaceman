import { ago, bytes, clock, isSet } from './format'

describe('format', () => {
  const now = Date.parse('2026-11-02T12:00:00Z')
  it('formats relative times', () => {
    expect(ago('2026-11-02T11:59:58Z', now)).toBe('now')
    expect(ago('2026-11-02T11:59:50Z', now)).toBe('10s ago')
    expect(ago('2026-11-02T12:00:08Z', now)).toBe('in 8s')
    expect(ago('2026-11-02T11:55:00Z', now)).toBe('5m ago')
    expect(ago('2026-11-02T15:00:00Z', now)).toBe('in 3h')
    expect(ago(Date.parse('2026-10-31T12:00:00Z') * 1000, now)).toBe('2d ago')
    expect(ago(undefined, now)).toBe('—')
  })
  it('formats clocks', () => {
    expect(clock(0)).toBe('0:00')
    expect(clock(65_000)).toBe('1:05')
    expect(clock(3_725_000)).toBe('1:02:05')
  })
  it('treats the Go zero time as unset', () => {
    expect(isSet('0001-01-01T00:00:00Z')).toBe(false)
    expect(isSet('2026-11-02T00:00:00Z')).toBe(true)
  })
  it('formats byte counts', () => {
    expect(bytes(512)).toBe('512 B')
    expect(bytes(1536)).toBe('1.5 KiB')
    expect(bytes(5 * 1024 * 1024)).toBe('5.0 MiB')
    expect(bytes(undefined)).toBe('—')
  })
})
