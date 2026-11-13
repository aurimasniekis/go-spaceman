import { MantineProvider } from '@mantine/core'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type RecordingRecord } from '@/api/client'
import type { RecordingSummary, Screen } from './records'
import { TerminalView } from './TerminalView'

// A terminal that records what is written to it (ghostty-web needs WebAssembly and a canvas).
const fake = vi.hoisted(() => {
  const log: string[] = []
  const sub = { dispose() {} }
  const term = {
    options: {},
    viewportY: 0,
    reset: () => log.push('RESET'),
    write: (d: string | Uint8Array) => log.push(typeof d === 'string' ? d : new TextDecoder().decode(d)),
    resize: () => {},
    onScroll: () => sub,
    onCursorMove: () => sub,
    onResize: () => sub,
    onData: () => sub,
    focus: () => {},
  }
  /** The feeds started by seeks: each applies its records only when the test finishes it. */
  const feeds: { finish(): void }[] = []
  return { log, term, feeds }
})

vi.mock('./ghostty', () => ({
  createTerminal: async () => fake.term,
  disposeTerminal: () => {},
  fitFont: () => {},
  followCursor: () => () => {},
  holdDrawing: () => () => {},
  regularCell: () => null,
  rowHeight: () => 0,
  scrollWithWheelAndTouch: () => () => {},
}))
vi.mock('./scroll', () => ({ jumpToLive: () => {}, keepingPosition: (_t: unknown, w: () => void) => w(), scrolledAway: () => false }))
vi.mock('./records', async (original) => {
  const actual = await original<typeof import('./records')>()
  return {
    ...actual,
    // A seek's feed that is still in progress until the test calls finish().
    feed: (screen: Screen, start: RecordingSummary, onSlice: (s: RecordingSummary, backlog: number) => void) => {
      const queued: RecordingRecord[] = []
      let stopped = false
      fake.feeds.push({
        finish() {
          if (stopped) return
          onSlice(queued.reduce((sum, r) => actual.apply(screen, sum, r), start), 0)
        },
      })
      return { push: (rs: readonly RecordingRecord[]) => void queued.push(...rs), backlog: () => queued.length, stop: () => void (stopped = true) }
    },
  }
})

const out = (seq: number, d: string): RecordingRecord => ({ seq, t: seq * 1_000_000, k: 'o', d: btoa(d) })
const records: RecordingRecord[] = [{ seq: 0, t: 0, k: 'open', cols: 80, rows: 24, run: 'run_1' }, out(1, 'a'), out(2, 'b'), out(3, 'c'), out(4, 'd')]

/** What the terminal shows: everything written since its last reset. */
const shown = () => fake.log.slice(fake.log.lastIndexOf('RESET') + 1).join('')
const flush = (ms = 0) => act(() => vi.advanceTimersByTimeAsync(ms))

beforeEach(() => {
  vi.useFakeTimers()
  fake.log.length = 0
  fake.feeds.length = 0
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  vi.spyOn(api, 'GET').mockResolvedValue({ data: { records, more: false, next_after: 0 }, response: new Response() } as never)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('ReplayTerminal', () => {
  it('never applies a pending playback step while a seek renders from a clean screen', async () => {
    render(<MantineProvider><TerminalView spaceId="spc_1" runId="run_1" mode="replay" /></MantineProvider>)
    await flush()
    // The whole recording is shown first.
    expect(fake.feeds).toHaveLength(1)
    await act(async () => fake.feeds[0].finish())
    expect(shown()).toBe('abcd')

    // Play restarts from the beginning: the open record, then 'a'; the next step is due a second later.
    fireEvent.click(screen.getByRole('button', { name: 'Play replay' }))
    await flush()
    await flush(1_000)
    expect(shown()).toBe('a')

    // A seek to the end while that step is pending: React cleans up the playback effect only after this task, so the
    // step's timer can fire first, onto the terminal the seek has just reset.
    act(() => {
      fireEvent.keyDown(screen.getByRole('slider'), { key: 'End' })
      vi.advanceTimersByTime(1_000)
    })
    expect(fake.feeds).toHaveLength(2)
    expect(shown()).toBe('')
    await act(async () => fake.feeds[1].finish())
    expect(shown()).toBe('abc')
  })
})

describe('tails (ADR-0081)', () => {
  it('a replay shows the tail first and does not draw it again when the whole recording has the same end', async () => {
    const tailPage = {
      records: [out(3, 'c'), out(4, 'd')],
      more: false,
      next_after: 4,
      tail: { from: 3, skipped_bytes: 2, cols: 100, rows: 30, modes: [{ mode: 1049, on: true }], gaps: 0, truncated: false },
    }
    // The whole recording arrives only when the test lets it.
    let release = () => {}
    const full = new Promise<void>((r) => (release = r))
    vi.spyOn(api, 'GET').mockImplementation((async (_path: string, opts: { params: { query: { tail?: number } } }) => {
      if (!opts.params.query.tail) await full
      return { data: opts.params.query.tail ? tailPage : { records, more: false, next_after: 4 }, response: new Response() }
    }) as never)
    render(<MantineProvider><TerminalView spaceId="spc_1" runId="run_1" mode="replay" /></MantineProvider>)
    await flush()
    expect(fake.feeds).toHaveLength(1)
    await act(async () => fake.feeds[0].finish())
    expect(shown()).toBe('\x1b[?1049hcd')
    expect(screen.queryByRole('slider')).toBeNull()
    release()
    await flush()
    // The whole recording arrived with the same last record: nothing is drawn again.
    expect(fake.feeds).toHaveLength(1)
    expect(screen.getByRole('slider')).toBeTruthy()
  })

  it('a replay draws the whole recording when it arrives before the tail is drawn', async () => {
    const tailPage = {
      records: [out(3, 'c'), out(4, 'd')],
      more: false,
      next_after: 4,
      tail: { from: 3, skipped_bytes: 2, cols: 0, rows: 0, modes: [], gaps: 0, truncated: false },
    }
    let release = () => {}
    const full = new Promise<void>((r) => (release = r))
    vi.spyOn(api, 'GET').mockImplementation((async (_path: string, opts: { params: { query: { tail?: number } } }) => {
      if (!opts.params.query.tail) await full
      return { data: opts.params.query.tail ? tailPage : { records, more: false, next_after: 4 }, response: new Response() }
    }) as never)
    render(<MantineProvider><TerminalView spaceId="spc_1" runId="run_1" mode="replay" /></MantineProvider>)
    await flush()
    expect(fake.feeds).toHaveLength(1)
    // The whole recording arrives while the tail is still being drawn (a slow device): its feed stops, and the
    // recording's end is drawn instead of being left half done.
    release()
    await flush()
    expect(fake.feeds).toHaveLength(2)
    await act(async () => fake.feeds[1].finish())
    expect(shown()).toBe('abcd')
    await act(async () => fake.feeds[0].finish())
    expect(shown()).toBe('abcd')
  })

  it('a live terminal asks for a tail and applies what comes before it first', async () => {
    const streams: { url: string; on: Record<string, (ev: MessageEvent) => void> }[] = []
    class FakeEventSource {
      static CLOSED = 2
      readyState = 1
      onopen: (() => void) | null = null
      onerror: (() => void) | null = null
      on: Record<string, (ev: MessageEvent) => void> = {}
      constructor(public url: string) {
        streams.push(this)
      }
      addEventListener(kind: string, fn: (ev: MessageEvent) => void) {
        this.on[kind] = fn
      }
      close() {}
    }
    vi.stubGlobal('EventSource', FakeEventSource)
    try {
      const { container } = render(<MantineProvider><TerminalView spaceId="spc_1" runId="run_1" mode="live" /></MantineProvider>)
      await flush()
      expect(streams).toHaveLength(1)
      expect(streams[0].url).toMatch(/\/terminal\?tail=1048576$/)
      const send = (kind: string, data: unknown) => act(() => streams[0].on[kind](new MessageEvent(kind, { data: JSON.stringify(data) })))
      send('tail', { from: 7, skipped_bytes: 5000, cols: 0, rows: 0, modes: [{ mode: 25, on: false }], gaps: 1, truncated: false })
      send('record', out(7, 'x'))
      send('record', out(8, 'y'))
      await act(async () => fake.feeds[0].finish())
      expect(shown()).toBe('\x1b[?25lxy')
      expect(container.querySelector('[data-earlier]')?.getAttribute('data-earlier')).toBe('5000')
    } finally {
      vi.unstubAllGlobals()
    }
  })
})
