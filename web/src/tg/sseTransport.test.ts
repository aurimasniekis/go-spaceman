import { afterEach, describe, expect, it, vi } from 'vitest'
import * as session from '@/api/session'
import type { StreamHandlers } from '@/api/transport'
import { fetchSseTransport, PROBLEM_BODY_WAIT_MS, SseParser, type SseEvent } from './sseTransport'

function parse(chunks: string[]): SseEvent[] {
  const out: SseEvent[] = []
  const p = new SseParser((e) => out.push(e))
  for (const c of chunks) p.push(c)
  return out
}

describe('SseParser', () => {
  it('parses events split across chunks, keeps the last id and ignores comments', () => {
    expect(parse([': hi\nid: 4\nevent: change\ndata: {"a"', ':1}\n\n', 'event: gap\ndata: x\ndata: y\n\n'])).toEqual([
      { type: 'change', data: '{"a":1}', id: '4' },
      { type: 'gap', data: 'x\ny', id: '4' },
    ])
  })
  it('handles CRLF and CR line ends, also split between chunks, and a BOM', () => {
    expect(parse(['﻿data: a\r\n\r\ndata: b\r', '\n\r', '\ndata: c\r\r'])).toEqual([
      { type: 'message', data: 'a', id: '' },
      { type: 'message', data: 'b', id: '' },
      { type: 'message', data: 'c', id: '' },
    ])
  })
  it('dispatches nothing without data', () => {
    expect(parse(['event: x\n\nid: 1\n\n'])).toEqual([])
  })
})

function stream(chunks: string[], init: ResponseInit = { status: 200, headers: { 'Content-Type': 'text/event-stream' } }): Response {
  const enc = new TextEncoder()
  return new Response(
    new ReadableStream({
      start(c) {
        for (const ch of chunks) c.enqueue(enc.encode(ch))
        c.close()
      },
    }),
    init,
  )
}

function handlers() {
  const calls: string[] = []
  const h: StreamHandlers = {
    onOpen: () => calls.push('open'),
    onEvent: (t, d, id) => calls.push(`${t}|${d}|${id}`),
    onReconnecting: () => calls.push('reconnecting'),
    onClosed: (s) => calls.push(`closed${s ? `:${s}` : ''}`),
  }
  return { calls, h }
}
const settle = () => new Promise((r) => setTimeout(r, 20))

describe('fetchSseTransport', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    session.clearTelegramSession()
  })

  it('sends the Telegram header with credentials omitted and delivers events', async () => {
    session.setTelegramSession('tok')
    const f = vi.fn().mockResolvedValue(stream(['event: change\nid: 7\ndata: {"x":1}\n\n']))
    vi.stubGlobal('fetch', f)
    const { calls, h } = handlers()
    fetchSseTransport.open('/api/v1/watch?live=1', h)
    await settle()
    const [url, init] = f.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/v1/watch?live=1')
    expect(init.credentials).toBe('omit')
    const hd = new Headers(init.headers)
    expect(hd.get('Authorization')).toBe('Telegram tok')
    expect(hd.get('Accept')).toBe('text/event-stream')
    expect(calls).toEqual(['open', 'change|{"x":1}|7', 'closed'])
  })

  it('ends the session on 401 and neither reports closed nor retries', async () => {
    const end = vi.fn()
    session.setMiniAppMode(end)
    const f = vi.fn().mockResolvedValue(new Response('{}', { status: 401 }))
    vi.stubGlobal('fetch', f)
    const { calls, h } = handlers()
    fetchSseTransport.open('/api/v1/watch', h)
    await settle()
    expect(end).toHaveBeenCalledOnce()
    expect(calls).toEqual([])
    expect(f).toHaveBeenCalledOnce()
  })

  it('ends the session with the MFA message on a 401 mfa_required from the stream', async () => {
    const end = vi.fn()
    session.setMiniAppMode(end)
    const f = vi.fn().mockResolvedValue(new Response(JSON.stringify({ code: 'mfa_required' }), { status: 401, headers: { 'Content-Type': 'application/problem+json' } }))
    vi.stubGlobal('fetch', f)
    const { calls, h } = handlers()
    fetchSseTransport.open('/api/v1/watch', h)
    await settle()
    expect(end).toHaveBeenCalledOnce()
    expect(end).toHaveBeenCalledWith(session.MFA_PENDING_MESSAGE)
    expect(calls).toEqual([])
    expect(f).toHaveBeenCalledOnce()
  })

  it('reports another status, or a wrong content type, as closed', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValueOnce(new Response('', { status: 502 })).mockResolvedValueOnce(new Response('x', { status: 200 })))
    const a = handlers()
    fetchSseTransport.open('/u', a.h)
    const b = handlers()
    fetchSseTransport.open('/u', b.h)
    await settle()
    expect(a.calls).toEqual(['closed:502'])
    expect(b.calls).toEqual(['closed:200'])
  })

  it('aborts the request on a 401 whose body never ends, once the bounded read gives up', async () => {
    const body = new ReadableStream({ start: (c) => c.enqueue(new TextEncoder().encode('{"code":')) })
    const f = vi.fn().mockResolvedValue(new Response(body, { status: 401 }))
    vi.stubGlobal('fetch', f)
    const end = vi.fn()
    session.setMiniAppMode(end)
    const { calls, h } = handlers()
    fetchSseTransport.open('/u', h)
    await vi.waitFor(() => expect(end).toHaveBeenCalledOnce(), { timeout: PROBLEM_BODY_WAIT_MS * 3 })
    const init = f.mock.calls[0][1] as RequestInit
    expect(init.signal?.aborted).toBe(true)
    expect(calls).toEqual([])
  })

  it.each([
    ['502', 502, 'text/event-stream'],
    ['a wrong content type', 200, 'text/plain'],
  ])('cancels the body and aborts the request on %s', async (_name, status, type) => {
    const cancelled = vi.fn()
    const body = new ReadableStream({ start: (c) => c.enqueue(new TextEncoder().encode('data: x\n\n')), cancel: cancelled })
    const f = vi.fn().mockResolvedValue(new Response(body, { status, headers: { 'Content-Type': type } }))
    vi.stubGlobal('fetch', f)
    session.setMiniAppMode(() => {})
    session.setTelegramSession('tok')
    const { h } = handlers()
    fetchSseTransport.open('/u', h)
    await settle()
    const init = f.mock.calls[0][1] as RequestInit
    expect(cancelled).toHaveBeenCalledOnce()
    expect(init.signal?.aborted).toBe(true)
  })

  it('stays quiet after close()', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new DOMException('x', 'AbortError')))
    const { calls, h } = handlers()
    fetchSseTransport.open('/u', h).close()
    await settle()
    expect(calls).toEqual([])
  })
})
