import { QueryClient, QueryObserver } from '@tanstack/react-query'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { keys, runsQuery } from './queries'
import { applyChange, INVALIDATE_WINDOW_MS, refresh } from './watch'

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

  // Review B1: a change while the list's refetch is in flight must not be lost. That fetch may have read the server
  // before the change, and its success would mark the old data fresh; the query is refetched once more after it.
  it('never caches data read before a change that arrived while it was being fetched', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 60_000 } } })
    let server = 0 // the runs list as the server has it
    let fetches = 0
    let inFlight = 0
    let maxInFlight = 0
    const queryFn = () => {
      const read = server // what this request reads, when it starts
      fetches++
      inFlight++
      maxInFlight = Math.max(maxInFlight, inFlight)
      return new Promise<number>((resolve) => setTimeout(() => {
        inFlight--
        resolve(read)
      }, 400))
    }
    const obs = new QueryObserver(qc, { queryKey: keys.runs('spc_a'), queryFn })
    const unsub = obs.subscribe(() => {})
    await vi.advanceTimersByTimeAsync(500)
    expect(obs.getCurrentResult().data).toBe(0)

    server = 1 // the run goes idle
    applyChange(qc, runChange('spc_a', 1))
    await vi.advanceTimersByTimeAsync(INVALIDATE_WINDOW_MS + 50) // that refetch is in flight, it read 1
    server = 2 // the run exits
    applyChange(qc, runChange('spc_a', 2))
    await vi.advanceTimersByTimeAsync(5000)
    expect(obs.getCurrentResult().data).toBe(2)
    expect(maxInFlight).toBe(1)
    expect(fetches).toBeLessThanOrEqual(4)
    unsub()
  })

  it('the position refresh also refetches a query that was in flight when it arrived', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 60_000 } } })
    let server = 0
    const queryFn = () => {
      const read = server
      return new Promise<number>((resolve) => setTimeout(() => resolve(read), 400))
    }
    const obs = new QueryObserver(qc, { queryKey: keys.spaces, queryFn })
    const unsub = obs.subscribe(() => {})
    await vi.advanceTimersByTimeAsync(100) // the first load is in flight, it read 0
    server = 1 // changed before the feed's position
    refresh(qc) // the position event
    await vi.advanceTimersByTimeAsync(2000)
    expect(obs.getCurrentResult().data).toBe(1)
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

  it('refreshes every space\'s usage-limit holds on a hold change, and only that space\'s waits on a wait change', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    for (const sp of ['spc_a', 'spc_b']) {
      qc.setQueryData(keys.usageLimits(sp), [])
      qc.setQueryData(keys.limitWaits(sp), [])
    }
    applyChange(qc, { seq: 1, at: 0, space_id: 'spc_a', resource_type: 'usage_limit', resource_id: 'ulm_1', action: 'created' })
    applyChange(qc, { seq: 2, at: 0, space_id: 'spc_a', resource_type: 'limit_wait', resource_id: 'lwt_1', action: 'created' })
    await vi.advanceTimersByTimeAsync(INVALIDATE_WINDOW_MS)
    const stale = (k: readonly unknown[]) => qc.getQueryState(k)?.isInvalidated
    // A hold on a shared account shows, redacted, in the other spaces bound to it.
    expect(stale(keys.usageLimits('spc_a'))).toBe(true)
    expect(stale(keys.usageLimits('spc_b'))).toBe(true)
    expect(stale(keys.limitWaits('spc_a'))).toBe(true)
    expect(stale(keys.limitWaits('spc_b'))).toBe(false)
  })
})
