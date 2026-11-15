import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { clearTelegramSession, setTelegramSession } from '@/api/session'
import { setWatchTransport, eventSourceTransport } from '@/api/transport'
import { useWatch } from '@/api/watch'
import { fetchSseTransport } from './sseTransport'

// The Mini App's fetch stream never reconnects by itself: when the proxy drops it, useWatch reopens it with backoff,
// resuming from the last event id.
const enc = new TextEncoder()

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } })
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>
}

/** A stream that delivers `first`, then stays open until the test drops it. */
function openStream(first: string) {
  let ctrl!: ReadableStreamDefaultController<Uint8Array>
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      ctrl = c
      c.enqueue(enc.encode(first))
    },
  })
  return { res: new Response(body, { status: 200, headers: { 'Content-Type': 'text/event-stream' } }), drop: () => ctrl.close() }
}

const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms))

beforeEach(() => {
  vi.useFakeTimers()
  vi.spyOn(Math, 'random').mockReturnValue(0.5)
  setTelegramSession('tok')
  setWatchTransport(fetchSseTransport)
})
afterEach(() => {
  setWatchTransport(eventSourceTransport)
  clearTelegramSession()
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

describe('Mini App watch stream', () => {
  it('reconnects with backoff after the stream drops, resuming from the last event', async () => {
    const first = openStream('event: change\nid: 7\ndata: {"seq":7,"at":0,"resource_type":"token","resource_id":"x","action":"updated"}\n\n')
    const second = openStream(': hi\n\n')
    const f = vi
      .fn()
      .mockResolvedValueOnce(first.res)
      .mockResolvedValueOnce(new Response('', { status: 502 })) // central still restarting: backs off again
      .mockResolvedValueOnce(second.res)
    vi.stubGlobal('fetch', f)

    const { result } = renderHook(() => useWatch(true), { wrapper })
    await tick()
    expect(result.current).toBe('live')
    expect(f).toHaveBeenCalledTimes(1)

    first.drop() // the proxy dropped the idle connection
    await tick()
    expect(result.current).toBe('offline')
    await tick(999)
    expect(f).toHaveBeenCalledTimes(1)
    await tick(1) // 1 s
    expect(f).toHaveBeenCalledTimes(2)
    expect(f.mock.calls[1][0]).toBe('/api/v1/watch?after=7')

    await tick(1999)
    expect(f).toHaveBeenCalledTimes(2)
    await tick(1) // 2 s: the backoff doubled
    expect(f).toHaveBeenCalledTimes(3)
    expect(f.mock.calls[2][0]).toBe('/api/v1/watch?after=7')
    await tick()
    expect(result.current).toBe('live')
  })
})
