import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, vi } from 'vitest'
import { retryDelay, RETRY_MAX_MS, useWatch } from './watch'

/** A scriptable EventSource: the test opens, errors, closes and ends each instance by hand. */
class FakeEventSource extends EventTarget {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  static all: FakeEventSource[] = []
  readonly CONNECTING = 0
  readonly OPEN = 1
  readonly CLOSED = 2
  readyState = 0
  onopen: ((ev: Event) => void) | null = null
  onerror: ((ev: Event) => void) | null = null
  constructor(readonly url: string) {
    super()
    FakeEventSource.all.push(this)
  }
  close() {
    this.readyState = 2
  }
  open() {
    this.readyState = 1
    this.onopen?.(new Event('open'))
  }
  /** EventSource gave up (a non-200 or non-event-stream response). */
  fail() {
    this.readyState = 2
    this.onerror?.(new Event('error'))
  }
  emit(type: string, data: string, lastEventId = '') {
    this.dispatchEvent(new MessageEvent(type, { data, lastEventId }))
  }
}

/** BroadcastChannel delivering synchronously to the other instances of the same name. */
class FakeChannel {
  static all: FakeChannel[] = []
  onmessage: ((ev: MessageEvent) => void) | null = null
  closed = false
  constructor(readonly name: string) {
    FakeChannel.all.push(this)
  }
  postMessage(data: unknown) {
    for (const c of FakeChannel.all) if (c !== this && !c.closed && c.name === this.name) c.onmessage?.(new MessageEvent('message', { data }))
  }
  close() {
    this.closed = true
  }
}

const latest = () => FakeEventSource.all[FakeEventSource.all.length - 1]
const assign = vi.fn()
let me: { status: number; body: unknown } | 'down'
const fetchMock = vi.fn(async () => {
  if (me === 'down') throw new TypeError('network')
  const { status, body } = me
  return { ok: status >= 200 && status < 300, status, json: async () => body }
})

function wrapper({ children }: { children: ReactNode }) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } })
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>
}

// Lets the session check's promises settle and runs due timers.
const tick = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms))

beforeEach(() => {
  vi.useFakeTimers()
  vi.spyOn(Math, 'random').mockReturnValue(0.5) // no jitter: delays are exactly 1 s, 2 s, 4 s …
  FakeEventSource.all = []
  FakeChannel.all = []
  me = { status: 200, body: { limited: false } }
  vi.stubGlobal('EventSource', FakeEventSource)
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('location', { ...window.location, pathname: '/spaces/spc_1', search: '', assign })
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  assign.mockReset()
  fetchMock.mockClear()
})

describe('retryDelay', () => {
  it('doubles from 1 s never more than 30 s, with ±20 % jitter', () => {
    expect([0, 1, 2, 3, 4, 5, 6].map((a) => retryDelay(a, 0.5))).toEqual([1000, 2000, 4000, 8000, 16000, 30000, 30000])
    expect(retryDelay(0, 0)).toBe(800)
    expect(retryDelay(0, 1)).toBe(1200)
    expect(retryDelay(20, 1)).toBe(RETRY_MAX_MS)
    expect(retryDelay(20, 0)).toBe(RETRY_MAX_MS * 0.8)
  })
})

describe('useWatch (one tab, no Web Locks)', () => {
  it('reopens a closed stream after the delay, resuming from the last change', async () => {
    const { result } = renderHook(() => useWatch(true), { wrapper })
    expect(FakeEventSource.all).toHaveLength(1)
    act(() => latest().open())
    expect(result.current).toBe('live')
    act(() => latest().emit('change', JSON.stringify({ seq: 7, at: 0, resource_type: 'token', resource_id: 'x', action: 'updated' }), '7'))

    act(() => latest().fail()) // a 502 while central restarts
    expect(result.current).toBe('offline')
    await tick(999)
    expect(FakeEventSource.all).toHaveLength(1)
    expect(fetchMock).not.toHaveBeenCalled()

    await tick(1)
    expect(fetchMock).toHaveBeenCalledTimes(1) // the session is re-checked first
    expect(FakeEventSource.all).toHaveLength(2)
    expect(latest().url).toBe('/api/v1/watch?after=7')
    expect(result.current).toBe('connecting')
    act(() => latest().open())
    expect(result.current).toBe('live')
  })

  it('grows the backoff while central is down and resets it once a stream opens', async () => {
    const { result } = renderHook(() => useWatch(true), { wrapper })
    const failNext = async (delay: number) => {
      act(() => latest().fail())
      expect(result.current).toBe('offline')
      const n = FakeEventSource.all.length
      await tick(delay - 1)
      expect(FakeEventSource.all).toHaveLength(n)
      await tick(1)
      expect(FakeEventSource.all).toHaveLength(n + 1)
    }
    await failNext(1000)
    await failNext(2000)
    await failNext(4000)

    // A failed session check (central unreachable) waits for the next step too.
    me = 'down'
    act(() => latest().fail())
    await tick(8000)
    expect(FakeEventSource.all).toHaveLength(4)
    expect(result.current).toBe('offline')
    me = { status: 200, body: { limited: false } }
    await tick(16000)
    expect(FakeEventSource.all).toHaveLength(5)

    act(() => latest().open())
    expect(result.current).toBe('live')
    await failNext(1000) // reset
  })

  it("retries after an end with no access reason ('')", async () => {
    const { result } = renderHook(() => useWatch(true), { wrapper })
    act(() => latest().open())
    act(() => latest().emit('end', '{}'))
    expect(result.current).toBe('offline')
    await tick(1000)
    expect(FakeEventSource.all).toHaveLength(2)
    act(() => latest().open())
    expect(result.current).toBe('live')
  })

  it('goes to sign-in when the stream ends as unauthenticated and the session is gone', async () => {
    renderHook(() => useWatch(true), { wrapper })
    act(() => latest().open())
    me = { status: 401, body: { code: 'unauthorized' } }
    act(() => latest().emit('end', '{"reason":"unauthenticated"}'))
    await tick()
    expect(fetchMock).toHaveBeenCalledTimes(1) // at once, without waiting for a backoff step
    expect(assign).toHaveBeenCalledWith('/login?return_to=%2Fspaces%2Fspc_1')
    expect(FakeEventSource.all).toHaveLength(1)
  })

  it('reconnects at once after an unauthenticated end with a rotated session', async () => {
    const { result } = renderHook(() => useWatch(true), { wrapper })
    act(() => latest().open())
    act(() => latest().emit('end', '{"reason":"unauthenticated"}'))
    await tick()
    expect(FakeEventSource.all).toHaveLength(2)
    expect(assign).not.toHaveBeenCalled()
    act(() => latest().open())
    expect(result.current).toBe('live')
  })

  it('backs off when a server keeps ending fresh streams as unauthenticated', async () => {
    renderHook(() => useWatch(true), { wrapper })
    const endNext = async (delay: number) => {
      act(() => latest().open())
      act(() => latest().emit('end', '{"reason":"unauthenticated"}'))
      const n = FakeEventSource.all.length
      if (delay > 0) {
        await tick(delay - 1)
        expect(FakeEventSource.all).toHaveLength(n)
      }
      await tick(delay > 0 ? 1 : 0)
      expect(FakeEventSource.all).toHaveLength(n + 1)
    }
    await endNext(0) // the first one reconnects at once
    await endNext(1000)
    await endNext(2000)
    await endNext(4000)
    expect(assign).not.toHaveBeenCalled()

    // A stream that stayed open for a while ends as unauthenticated: at once again.
    act(() => latest().open())
    await tick(RETRY_MAX_MS)
    act(() => latest().emit('end', '{"reason":"unauthenticated"}'))
    await tick()
    expect(FakeEventSource.all).toHaveLength(6)
  })

  it('retries at once when the window comes back online', async () => {
    renderHook(() => useWatch(true), { wrapper })
    act(() => latest().fail())
    act(() => window.dispatchEvent(new Event('online')))
    await tick()
    expect(FakeEventSource.all).toHaveLength(2)
    // The pending backoff timer was dropped: no second stream later.
    await tick(5000)
    expect(FakeEventSource.all).toHaveLength(2)
  })

  it('stops retrying and drops its listeners on cleanup', async () => {
    const { unmount } = renderHook(() => useWatch(true), { wrapper })
    act(() => latest().fail())
    unmount()
    window.dispatchEvent(new Event('online'))
    await tick(60_000)
    expect(FakeEventSource.all).toHaveLength(1)
    expect(fetchMock).not.toHaveBeenCalled()
  })
})

describe('useWatch (leading tab)', () => {
  let signal: AbortSignal | undefined
  beforeEach(() => {
    signal = undefined
    vi.stubGlobal('BroadcastChannel', FakeChannel)
    // A lock granted at once; the callback's promise is the hold.
    const locks = {
      request: (name: string, opts: { signal?: AbortSignal }, cb: () => Promise<void>) => {
        expect(name).toBe('spaceman-watch.v2')
        signal = opts.signal
        return cb()
      },
    }
    Object.defineProperty(navigator, 'locks', { value: locks, configurable: true })
  })
  afterEach(() => {
    Reflect.deleteProperty(navigator, 'locks')
  })

  it('retries at once when another tab says hello while it is offline', async () => {
    const { result } = renderHook(() => useWatch(true), { wrapper })
    await tick()
    expect(FakeChannel.all[0].name).toBe('spaceman-watch.v2')
    act(() => latest().fail())
    expect(result.current).toBe('offline')

    const follower = new FakeChannel('spaceman-watch.v2')
    const seen: unknown[] = []
    follower.onmessage = (ev) => seen.push(ev.data)
    act(() => follower.postMessage({ t: 'hello' }))
    await tick()
    expect(seen[0]).toEqual({ t: 'state', s: 'offline' })
    expect(FakeEventSource.all).toHaveLength(2)
    expect(seen).toContainEqual({ t: 'state', s: 'connecting' })
    act(() => latest().open())
    expect(result.current).toBe('live')
    expect(seen[seen.length - 1]).toEqual({ t: 'state', s: 'live' })

    // A hello while live doesn't open a second stream.
    act(() => follower.postMessage({ t: 'hello' }))
    await tick()
    expect(FakeEventSource.all).toHaveLength(2)
  })

  it('retries at once when it becomes visible again', async () => {
    renderHook(() => useWatch(true), { wrapper })
    await tick()
    act(() => latest().fail())
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    await tick()
    expect(FakeEventSource.all).toHaveLength(2)
    Reflect.deleteProperty(document, 'visibilityState')
  })

  it('stops retrying when the lock is released', async () => {
    const { unmount } = renderHook(() => useWatch(true), { wrapper })
    await tick()
    act(() => latest().fail())
    unmount() // aborts the lock request: the hold ends
    expect(signal?.aborted).toBe(true)
    window.dispatchEvent(new Event('online'))
    await tick(60_000)
    expect(FakeEventSource.all).toHaveLength(1)
  })
})

describe('useWatch (a fresh tab)', () => {
  it('starts live instead of replaying the history, resumes from the position and refreshes its queries once', async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity } } })
    const invalidate = vi.spyOn(qc, 'invalidateQueries')
    renderHook(() => useWatch(true), { wrapper: ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider> })
    expect(latest().url).toBe('/api/v1/watch?live=1')
    act(() => latest().open())
    act(() => latest().emit('position', '{"seq":4200}', '4200'))
    expect(invalidate).toHaveBeenCalledTimes(1)
    expect(invalidate).toHaveBeenCalledWith(undefined, { cancelRefetch: false })

    act(() => latest().fail())
    await tick(1000)
    expect(latest().url).toBe('/api/v1/watch?after=4200')
  })
})
