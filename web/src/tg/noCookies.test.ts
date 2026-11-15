import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { StreamHandlers } from '@/api/transport'

// ADR-0083 §3.4.1: the Mini App never sends or keeps a cookie. Every request it makes (the typed API client, the
// attachment fetcher and the watch stream) carries credentials: 'omit' and no auth but `Authorization: Telegram <token>`.
// This pins every API helper the Mini App uses; a new helper that calls fetch must be added here.

type Seen = { url: string; credentials: string; headers: Headers }

let seen: Seen[]

function record(input: RequestInfo | URL, init?: RequestInit): Seen {
  if (input instanceof Request) return { url: input.url, credentials: input.credentials, headers: input.headers }
  return { url: String(input), credentials: init?.credentials ?? 'same-origin', headers: new Headers(init?.headers) }
}

function expectCookieless(s: Seen) {
  expect(s.credentials, s.url).toBe('omit')
  expect(s.headers.get('Authorization'), s.url).toBe('Telegram tok')
  expect(s.headers.has('Cookie'), s.url).toBe(false)
  expect(s.headers.has('X-CSRF-Token'), s.url).toBe(false)
  expect(s.headers.has('X-Csrf-Token'), s.url).toBe(false)
}

// Fresh modules per test, imported after the globals are stubbed (the API client captures Request and fetch).
let api: typeof import('@/api/client').api
let AttachmentBlobs: typeof import('./attachments').AttachmentBlobs
let fetchSseTransport: typeof import('./sseTransport').fetchSseTransport

beforeEach(async () => {
  seen = []
  vi.resetModules()
  // The API client builds Requests from relative URLs, which Node's Request refuses (a browser resolves them).
  const Base = Request
  vi.stubGlobal(
    'Request',
    class extends Base {
      constructor(input: RequestInfo | URL, init?: RequestInit) {
        super(typeof input === 'string' && input.startsWith('/') ? `http://tg.test${input}` : input, init)
      }
    },
  )
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(record(input, init))
      return new Response('{}', { status: 200, headers: { 'Content-Type': 'application/json' } })
    }),
  )
  const session = await import('@/api/session')
  session.setMiniAppMode(() => {})
  session.setTelegramSession('tok')
  api = (await import('@/api/client')).api
  AttachmentBlobs = (await import('./attachments')).AttachmentBlobs
  fetchSseTransport = (await import('./sseTransport')).fetchSseTransport
})
afterEach(() => {
  vi.unstubAllGlobals()
})

describe('Mini App requests carry no cookies', () => {
  it('the typed API client (reads and writes) omits credentials and sends only the Telegram header', async () => {
    await api.GET('/auth/me')
    await api.POST('/telegram/session', { body: { init_data: 'x' } })
    await api.POST('/telegram/download-links', { body: { space_id: 'spc_1', attachment_id: 'att_1' } })
    await api.GET('/spaces')
    expect(seen).toHaveLength(4)
    for (const s of seen) expectCookieless(s)
  })

  it('the attachment fetcher omits credentials', async () => {
    const f = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      seen.push(record(input, init))
      return new Response('', { status: 404 })
    })
    const blobs = new AttachmentBlobs(f as unknown as typeof fetch)
    void blobs.load({ spaceId: 'spc_1', id: 'att_1' })
    await vi.waitFor(() => expect(seen.length).toBeGreaterThan(0))
    for (const s of seen) expectCookieless(s)
  })

  it('the watch stream omits credentials', async () => {
    const h: StreamHandlers = { onOpen: () => {}, onEvent: () => {}, onReconnecting: () => {}, onClosed: () => {} }
    fetchSseTransport.open('/api/v1/watch?live=1', h).close()
    await vi.waitFor(() => expect(seen).toHaveLength(1))
    expectCookieless(seen[0])
  })

  it('the shared attachment upload omits credentials in Mini App mode', async () => {
    const { uploadAttachment } = await import('@/components/composer/upload')
    await uploadAttachment('spc_1', new File(['x'], 'a.txt', { type: 'text/plain' }))
    expect(seen).toHaveLength(1)
    expectCookieless(seen[0])
  })
})
