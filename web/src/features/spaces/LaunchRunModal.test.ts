import { describe, expect, it } from 'vitest'
import { spaceRoleOptions } from './LaunchRunModal'

describe('space role picker', () => {
  it('keeps a role called custom apart from the "another role" choice', () => {
    const values = spaceRoleOptions([{ name: 'control' }, { name: 'custom' }]).map((o) => o.value)
    expect(values).toContain('custom')
    expect(new Set(values).size).toBe(values.length)
  })
})
