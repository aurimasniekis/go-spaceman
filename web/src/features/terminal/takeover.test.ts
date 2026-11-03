import { describe, expect, it } from 'vitest'
import type { Me } from '@/api/client'
import { holderRef } from './Takeover'

// SM-UI-004: the page types only when the server's stored holder is the viewer (the API checks the same).
describe('holderRef', () => {
  const base = { domains: [], expires_at: '', limited: false, method: 'password', needs_enroll: false, session_id: 's' }
  it('names a user and the root token as the server stores them', () => {
    expect(holderRef({ ...base, root: false, user: { id: 'usr_1' } } as unknown as Me)).toBe('user:usr_1')
    expect(holderRef({ ...base, root: true } as unknown as Me)).toBe('root')
    expect(holderRef(undefined)).toBe('')
    expect(holderRef({ ...base, root: false } as unknown as Me)).toBe('')
  })
})
