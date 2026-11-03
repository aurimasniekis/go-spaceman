import { afterEach, vi } from 'vitest'
import { endReason, sessionStillValid } from './streams'

const end = (data: string) => new MessageEvent('end', { data })

describe('endReason', () => {
  it('reads the access reasons and treats anything else as a normal end', () => {
    expect(endReason(end('{"reason":"unauthenticated"}'))).toBe('unauthenticated')
    expect(endReason(end('{"reason":"forbidden"}'))).toBe('forbidden')
    expect(endReason(end('{}'))).toBe('')
    expect(endReason(end('{"reason":"other"}'))).toBe('')
    expect(endReason(end('not json'))).toBe('')
  })
})

describe('sessionStillValid', () => {
  const assign = vi.fn()
  beforeEach(() => {
    vi.stubGlobal('location', { ...window.location, pathname: '/spaces/spc_1', search: '', assign })
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    assign.mockReset()
  })
  const reply = (status: number, body: unknown) =>
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })))

  it('reconnects with a rotated (still valid) session', async () => {
    reply(200, { limited: false })
    expect(await sessionStillValid()).toBe(true)
    expect(assign).not.toHaveBeenCalled()
  })
  it('goes to sign-in when the session is gone', async () => {
    reply(401, { code: 'unauthorized' })
    expect(await sessionStillValid()).toBe(false)
    expect(assign).toHaveBeenCalledWith('/login?return_to=%2Fspaces%2Fspc_1')
  })
  it('goes to sign-in when the session still needs MFA', async () => {
    reply(200, { limited: true })
    expect(await sessionStillValid()).toBe(false)
    expect(assign).toHaveBeenCalled()
  })
  it('stays on the page when the server is unreachable', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => Promise.reject(new TypeError('network'))))
    expect(await sessionStillValid()).toBe(false)
    expect(assign).not.toHaveBeenCalled()
  })
})
