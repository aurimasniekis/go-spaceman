import type { Me } from '@/api/client'
import { atLeast, hasGlobal, roleIn } from './access'

const me = (grants: { role: string; domain: string }[]): Me =>
  ({ root: false, method: 'password', session_id: 's', limited: false, needs_enroll: false, expires_at: '', domains: [],
    user: { grants: grants.map((g) => ({ ...g, subject: 'user:u', source: 'manual' })) } }) as unknown as Me

describe('access', () => {
  it('takes the highest role across the space and global grants', () => {
    const m = me([{ role: 'viewer', domain: '*' }, { role: 'operator', domain: 'spc_1' }])
    expect(roleIn(m, 'spc_1')).toBe('operator')
    expect(roleIn(m, 'spc_2')).toBe('viewer')
    expect(atLeast(roleIn(m, 'spc_2'), 'operator')).toBe(false)
    expect(hasGlobal(m)).toBe(true)
    expect(hasGlobal(m, 'admin')).toBe(false)
  })
  it('has no role without grants', () => {
    expect(roleIn(me([]), 'spc_1')).toBeNull()
    expect(roleIn(null, 'spc_1')).toBeNull()
  })
})
