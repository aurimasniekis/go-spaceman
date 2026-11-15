import { afterEach, describe, expect, it, vi } from 'vitest'
import { clearTelegramSession, endMiniAppSession, MFA_PENDING_MESSAGE, miniAppFetchInit, setMiniAppMode, setTelegramSession } from './session'

afterEach(() => clearTelegramSession())

describe('miniAppFetchInit', () => {
  it('omits credentials and sends the Telegram header', () => {
    setTelegramSession('tok')
    const init = miniAppFetchInit()
    expect(init.credentials).toBe('omit')
    expect(new Headers(init.headers).get('Authorization')).toBe('Telegram tok')
  })

  it('omits the header (not "Telegram ") while there is no token', () => {
    const init = miniAppFetchInit({ headers: { Accept: 'x' } })
    const h = new Headers(init.headers)
    expect(h.has('Authorization')).toBe(false)
    expect(h.get('Accept')).toBe('x')
    expect(init.credentials).toBe('omit')
  })

  it('keeps the rest of init and overrides a caller credentials/Authorization', () => {
    setTelegramSession('tok')
    const init = miniAppFetchInit({ method: 'POST', credentials: 'include', headers: { Authorization: 'Bearer z' } })
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('omit')
    expect(new Headers(init.headers).get('Authorization')).toBe('Telegram tok')
  })
})

describe('endMiniAppSession', () => {
  it('ends the session; mfa_required carries the MFA message', async () => {
    const ended = vi.fn()
    setMiniAppMode(ended)
    await endMiniAppSession(new Response('{}', { status: 401 }))
    expect(ended).toHaveBeenLastCalledWith(undefined)
    await endMiniAppSession(new Response(JSON.stringify({ code: 'mfa_required' }), { status: 401 }))
    expect(ended).toHaveBeenLastCalledWith(MFA_PENDING_MESSAGE)
  })
})
