import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { eventSourceTransport, setWatchTransport, type StreamHandlers, type WatchTransport } from './transport'
import { useWatch } from './watch'

// useWatch over a transport other than EventSource (the Mini App's fetch transport plugs in the same way).
function fake() {
  const opened: { url: string; h: StreamHandlers; closed: boolean }[] = []
  let valid = true
  const t: WatchTransport = {
    supported: () => true,
    open: (url, h) => {
      const o = { url, h, closed: false }
      opened.push(o)
      return { close: () => (o.closed = true) }
    },
    sessionStillValid: () => Promise.resolve(valid),
  }
  return { t, opened, setValid: (v: boolean) => (valid = v) }
}
const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={new QueryClient()}>{children}</QueryClientProvider>
)
const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms))

beforeEach(() => vi.useFakeTimers())
afterEach(() => {
  vi.useRealTimers()
  setWatchTransport(eventSourceTransport)
})

describe('useWatch with another transport', () => {
  it('opens through it, goes live and resumes from the last change after the stream closes', async () => {
    const f = fake()
    setWatchTransport(f.t)
    const { result } = renderHook(() => useWatch(true), { wrapper })
    expect(f.opened[0].url).toBe('/api/v1/watch?live=1')
    act(() => f.opened[0].h.onOpen())
    expect(result.current).toBe('live')
    act(() => f.opened[0].h.onEvent('change', JSON.stringify({ seq: 5, at: 0, resource_type: 'token', resource_id: 'x', action: 'updated' }), '5'))
    act(() => f.opened[0].h.onClosed(502))
    expect(result.current).toBe('offline')
    await tick(1000)
    expect(f.opened[1].url).toBe('/api/v1/watch?after=5')
  })

  it('does not reopen while the transport says the session is gone', async () => {
    const f = fake()
    setWatchTransport(f.t)
    renderHook(() => useWatch(true), { wrapper })
    f.setValid(false)
    act(() => f.opened[0].h.onClosed())
    await tick(60_000)
    expect(f.opened).toHaveLength(1)
  })

  it('stays off where the transport is unsupported', () => {
    const f = fake()
    setWatchTransport({ ...f.t, supported: () => false })
    renderHook(() => useWatch(true), { wrapper })
    expect(f.opened).toHaveLength(0)
  })
})
