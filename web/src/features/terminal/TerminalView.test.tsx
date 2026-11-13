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
