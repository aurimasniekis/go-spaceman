import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { keys, runsQuery } from './queries'
import { applyChange, INVALIDATE_WINDOW_MS } from './watch'

const runChange = (space: string, i: number) => ({ seq: i, at: 0, space_id: space, resource_type: 'run', resource_id: `run_${i}`, action: 'updated' })

describe('change-feed invalidation', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
  })

  // Incident regression: a node reconnecting re-reported hundreds of runs; each change refetched the space's runs list
  // and cancelled the previous fetch without aborting its request, so a browser tab held hundreds of them open.
  it('refetches a watched runs list once for a burst of changes, never cancelling the fetch in flight', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0 } } })
    let fetches = 0
    let inFlight = 0
    let maxInFlight = 0
    const queryFn = () => {
      fetches++
      inFlight++
      maxInFlight = Math.max(maxInFlight, inFlight)
      return new Promise<string[]>((resolve) => setTimeout(() => {
        inFlight--
        resolve([])
      }, 400))
    }
    const unsub = new QueryObserver(qc, { queryKey: keys.runs('spc_a'), queryFn }).subscribe(() => {})
    await vi.advanceTimersByTimeAsync(500)
    expect(fetches).toBe(1)

    for (let i = 0; i < 1000; i++) applyChange(qc, runChange('spc_a', i))
    expect(fetches).toBe(1) // nothing yet: the window collects them
    await vi.advanceTimersByTimeAsync(INVALIDATE_WINDOW_MS)
    expect(fetches).toBe(2)
    // More changes while that refetch is in flight reuse it instead of starting another request.
    for (let i = 0; i < 1000; i++) applyChange(qc, runChange('spc_a', i))
    await vi.advanceTimersByTimeAsync(INVALIDATE_WINDOW_MS)
    expect(maxInFlight).toBe(1)
    await vi.advanceTimersByTimeAsync(2000)
    expect(fetches).toBeLessThanOrEqual(3)
    unsub()
  })

  it('invalidates each affected key once per window, other spaces included', async () => {
    const qc = new QueryClient()
    const spy = vi.spyOn(qc, 'invalidateQueries')
    for (let i = 0; i < 500; i++) {
      applyChange(qc, runChange('spc_a', i))
      applyChange(qc, runChange('spc_b', i))
    }
    expect(spy).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(INVALIDATE_WINDOW_MS)
    const calls = (space: string) => spy.mock.calls.filter(([f]) => JSON.stringify(f?.queryKey) === JSON.stringify(keys.runs(space)))
    expect(calls('spc_a')).toHaveLength(1)
    expect(calls('spc_b')).toHaveLength(1)
    for (const [, opts] of spy.mock.calls) expect(opts).toEqual({ cancelRefetch: false })
  })

  it('aborts a runs list request nobody waits for any more', async () => {
    vi.useRealTimers()
    let signal: AbortSignal | undefined
    const get = vi.spyOn(api, 'GET').mockImplementation(((_path: string, init: { signal?: AbortSignal }) => {
      signal = init.signal
      return new Promise((_, reject) => init.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError'))))
    }) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const p = qc.fetchQuery(runsQuery('spc_a')).catch(() => undefined)
    await new Promise((r) => setTimeout(r, 0))
    expect(get).toHaveBeenCalledTimes(1)
    expect(signal?.aborted).toBe(false)
    await qc.cancelQueries({ queryKey: keys.runs('spc_a') })
    await p
    expect(signal?.aborted).toBe(true)
    get.mockRestore()
  })
})
