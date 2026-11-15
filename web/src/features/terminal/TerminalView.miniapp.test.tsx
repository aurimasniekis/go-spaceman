import { MantineProvider } from '@mantine/core'
import { act, render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'

// ghostty-web needs WebAssembly and a canvas: a terminal that records what is wired to it.
const fake = vi.hoisted(() => {
  const sub = { dispose() {} }
  const term = {
    options: {} as { disableStdin?: boolean },
    viewportY: 0,
    reset() {},
    write() {},
    resize() {},
    onScroll: () => sub,
    onCursorMove: () => sub,
    onResize: () => sub,
    onData: vi.fn(() => sub),
    focus() {},
  }
  return { term }
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
  setInputMode: () => {},
}))

afterEach(() => {
  vi.unstubAllGlobals()
  fake.term.onData.mockClear()
})

async function mount(mode: 'live' | 'replay', extra: Record<string, unknown> = {}) {
  vi.resetModules()
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  const es = vi.fn()
  vi.stubGlobal('EventSource', es)
  const { setMiniAppMode } = await import('@/api/session')
  setMiniAppMode(() => undefined)
  const transport = await import('@/api/transport')
  const open = vi.fn((_url: string, _h: unknown, _types?: readonly string[]) => ({ close() {} }))
  transport.setWatchTransport({ supported: () => true, open, sessionStillValid: () => Promise.resolve(true) })
  const { TerminalView } = await import('./TerminalView')
  const view = render(
    <MantineProvider>
      <TerminalView spaceId="spc_1" runId="run_1" mode={mode} {...(extra as object)} />
    </MantineProvider>,
  )
  await act(async () => {})
  return { es, open, view }
}

// TG-10-3: the Mini App's terminal is the live stream over the Mini App's own transport, and only watches.
describe('TerminalView in the Mini App', () => {
  it('streams the live terminal over the Mini App transport, never EventSource', async () => {
    const { es, open } = await mount('live')
    expect(es).not.toHaveBeenCalled()
    expect(open).toHaveBeenCalledTimes(1)
    expect(open.mock.calls[0][0]).toMatch(/\/spaces\/spc_1\/runs\/run_1\/terminal(\?|$)/)
    expect(screen.getByLabelText('Run terminal (watch-only)')).toBeTruthy()
  })

  it('has no input path: no input element, no key handler, no size menu, even when a takeover handler is passed', async () => {
    const onInput = vi.fn()
    const { view } = await mount('live', { onInput, resize: { run: {}, blocker: '' } })
    expect(view.container.querySelectorAll('input, textarea, select, [contenteditable="true"]')).toHaveLength(0)
    expect(fake.term.onData).not.toHaveBeenCalled()
    expect(screen.queryByLabelText(/taken over/)).toBeNull()
    expect(screen.queryByText(/Size/i)).toBeNull()
    expect(onInput).not.toHaveBeenCalled()
  })

  it('has no replay', async () => {
    const { open } = await mount('replay')
    expect(open).not.toHaveBeenCalled()
    expect(screen.getByText(/not available in the Telegram app/)).toBeTruthy()
  })
})
