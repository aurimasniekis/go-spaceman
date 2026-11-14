import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

// A fresh module per test: the exchange runs once per page load.
async function load(initData: string | undefined, respond: () => Promise<Response>) {
  vi.resetModules()
  ;(window as unknown as { Telegram?: unknown }).Telegram = initData === undefined ? undefined : { WebApp: { initData, close: vi.fn() } }
  const f = vi.fn((_r: Request) => respond())
  vi.stubGlobal('fetch', f)
  // The API client builds Requests from relative URLs, which Node's Request refuses (a browser resolves them).
  const Base = Request
  vi.stubGlobal('Request', class extends Base {
    constructor(input: RequestInfo | URL, init?: RequestInit) {
      super(typeof input === 'string' && input.startsWith('/') ? `http://tg.test${input}` : input, init)
    }
  })
  const tg = await import('./session')
  const s = await import('@/api/session')
  return { tg, s, f }
}
const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('Mini App session', () => {
  beforeEach(() => vi.unstubAllGlobals())
  afterEach(() => vi.unstubAllGlobals())

  it('exchanges initData once, cookieless, then sends the Telegram header', async () => {
    const { tg, s, f } = await load('q=1&hash=x', () => Promise.resolve(json(201, { token: 'spaceman_tgs_abc', expires_at: 'x', idle_timeout_seconds: 1 })))
    await Promise.all([tg.startSession(), tg.startSession()])
    expect(f).toHaveBeenCalledOnce()
    const req = f.mock.calls[0][0] as Request
    expect(req.method).toBe('POST')
    expect(req.credentials).toBe('omit')
    expect(req.headers.get('Authorization')).toBeNull()
    expect(await req.json()).toEqual({ init_data: 'q=1&hash=x' })
    expect(s.telegramToken()).toBe('spaceman_tgs_abc')

    // A later API call carries the header and no cookie.
    const { api } = await import('@/api/client')
    f.mockResolvedValueOnce(json(200, []))
    await api.GET('/spaces')
    const next = f.mock.calls[1][0] as Request
    expect(next.headers.get('Authorization')).toBe('Telegram spaceman_tgs_abc')
    expect(next.credentials).toBe('omit')
  })

  it('shows the reopen state on a 401 from the exchange and does not retry', async () => {
    const { tg, f } = await load('q=1', () => Promise.resolve(json(401, { code: 'unauthorized' })))
    await tg.startSession()
    expect(f).toHaveBeenCalledOnce()
    expect(tg.isSessionEnded()).toBe(true)
  })

  it('ends the session when a later call gets 401, without a sign-in redirect or a retry', async () => {
    const { tg, f } = await load('q=1', () => Promise.resolve(json(201, { token: 't', expires_at: 'x', idle_timeout_seconds: 1 })))
    await tg.startSession()
    const { api } = await import('@/api/client')
    f.mockResolvedValueOnce(json(401, { code: 'unauthorized' }))
    await api.GET('/spaces')
    expect(f).toHaveBeenCalledTimes(2)
    expect(tg.isSessionEnded()).toBe(true)
  })

  it('says to finish MFA in the browser on a 403 from the exchange and on a later 401 mfa_required', async () => {
    const detail = 'multi-factor authentication is required for your SpaceMan account: finish setting it up in the browser'
    const a = await load('q=1', () => Promise.resolve(json(403, { code: 'forbidden', detail })))
    await a.tg.startSession()
    expect(a.tg.isSessionEnded()).toBe(true)

    const b = await load('q=1', () => Promise.resolve(json(201, { token: 't', expires_at: 'x', idle_timeout_seconds: 1 })))
    await b.tg.startSession()
    const { api } = await import('@/api/client')
    b.f.mockResolvedValueOnce(json(401, { code: 'mfa_required' }))
    await api.GET('/spaces')
    expect(b.tg.isSessionEnded()).toBe(true)
    // The message names the browser, not the generic reopen text.
    const { MFA_PENDING_MESSAGE } = await import('@/api/session')
    expect(MFA_PENDING_MESSAGE).toContain('browser')
  })

  it("shows the server's detail only for a 403 from the exchange", async () => {
    const { renderHook } = await import('@testing-library/react')
    const detail = 'a server-side detail naming the browser'
    const reason = async (status: number) => {
      const a = await load('q=1', () => Promise.resolve(json(status, { code: 'x', detail })))
      await a.tg.startSession()
      const { result } = renderHook(() => a.tg.useTgSession())
      return result.current.status === 'ended' ? result.current.reason : ''
    }
    expect(await reason(403)).toBe(detail)
    for (const status of [401, 429, 500]) {
      const r = await reason(status)
      expect(r).not.toContain(detail)
      expect(r).toBe('Session ended. Close and reopen the app.')
    }
  })

  it('reads its identity from /telegram/me, not /auth/me, and sends no CSRF token', async () => {
    const { tg, f } = await load('q=1', () => Promise.resolve(json(201, { token: 't', expires_at: 'x', idle_timeout_seconds: 1 })))
    await tg.startSession()
    const { QueryClient } = await import('@tanstack/react-query')
    const queryClient = new QueryClient()
    const { meQuery } = await import('@/api/queries')
    f.mockResolvedValueOnce(json(200, { root: false, method: 'telegram', session_id: '', limited: false, needs_enroll: false, expires_at: 'x', domains: [] }))
    const me = await queryClient.fetchQuery(meQuery)
    expect(me?.method).toBe('telegram')
    expect(new URL((f.mock.calls[1][0] as Request).url).pathname).toBe('/api/v1/telegram/me')
  })

  it('outside Telegram there is no exchange, only the reopen state', async () => {
    const { tg, f } = await load(undefined, () => Promise.reject(new Error('no')))
    await tg.startSession()
    expect(f).not.toHaveBeenCalled()
    expect(tg.isSessionEnded()).toBe(true)
  })
})
