import { describe, expect, it } from 'vitest'
import { rangeOf } from './range'

const now = Date.parse('2026-11-16T12:34:56Z')
describe('rangeOf', () => {
  it('ends after the current UTC hour or day', () => {
    expect(rangeOf('24h', now)).toEqual({ from: '2026-11-15T13:00:00Z', to: '2026-11-16T13:00:00Z', bucket: 'hour' })
    expect(rangeOf('7d', now)).toEqual({ from: '2026-11-10T00:00:00Z', to: '2026-11-17T00:00:00Z', bucket: 'day' })
    expect(rangeOf('30d', now).from).toBe('2026-10-18T00:00:00Z')
    expect(rangeOf('90d', now)).toEqual({ from: '2026-08-19T00:00:00Z', to: '2026-11-17T00:00:00Z', bucket: 'day' })
  })
})
