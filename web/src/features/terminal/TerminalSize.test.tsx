import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createRef, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Run } from '@/api/client'
import { choiceKey, fitDimensions, inBounds, loadChoice, parseChoice, resizeBlocker, saveChoice, sizeLabel, TerminalSizeControl } from './TerminalSize'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  window.localStorage.clear()
})
afterEach(() => vi.restoreAllMocks())

const run = (extra: Partial<Run> = {}): Run => ({ id: 'run_1', space_id: 'spc_1', desired: 'running', observed: 'running', terminal_sizing: true, ...extra }) as Run

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  return render(<MantineProvider><QueryClientProvider client={qc}>{ui}</QueryClientProvider></MantineProvider>)
}

describe('terminal size choices', () => {
  it('parse, bound and remember', () => {
    expect(parseChoice('120x40')).toEqual({ kind: 'fixed', cols: 120, rows: 40 })
    expect(parseChoice('default')).toEqual({ kind: 'default' })
    expect(parseChoice('39x24')).toEqual({ kind: 'fit' })
    expect(parseChoice('80x201')).toEqual({ kind: 'fit' })
    expect(parseChoice(null)).toEqual({ kind: 'fit' })
    expect(parseChoice('junk')).toEqual({ kind: 'fit' })
    expect(inBounds(40, 10) && inBounds(400, 200)).toBe(true)
    expect(inBounds(80.5, 24) || inBounds(401, 24) || inBounds(80, 9)).toBe(false)
    expect(loadChoice()).toEqual({ kind: 'fit' })
    saveChoice({ kind: 'fixed', cols: 160, rows: 50 })
    expect(choiceKey(loadChoice())).toBe('160x50')
  })

  it('fit the area at the regular cell size, within the bounds', () => {
    expect(fitDimensions(1200, 700, 8, 17)).toEqual({ cols: 150, rows: 41 })
    expect(fitDimensions(200, 100, 8, 17)).toEqual({ cols: 40, rows: 10 })
    expect(fitDimensions(10000, 10000, 8, 17)).toEqual({ cols: 400, rows: 200 })
    expect(fitDimensions(0, 700, 8, 17)).toBeNull()
  })

  it('say who may resize', () => {
    expect(resizeBlocker(run(), true, 'user:1', false)).toBe('')
    expect(resizeBlocker(run(), false, 'user:1', false)).toMatch(/takeover permission/)
    expect(resizeBlocker(run({ terminal_sizing: false }), true, 'user:1', false)).toMatch(/too old/)
    expect(resizeBlocker(run(), true, 'user:1', true)).toMatch(/ended/)
    const takeover = { id: 'tko_1', holder: 'user:2', holder_label: 'Bob' } as Run['takeover']
    expect(resizeBlocker(run({ takeover }), true, 'user:1', false)).toMatch(/Bob has taken over/)
    expect(resizeBlocker(run({ takeover }), true, 'user:2', false)).toBe('')
    expect(sizeLabel(run())).toBe('Node default')
    expect(sizeLabel(run({ terminal_size: { cols: 120, rows: 40 } }))).toBe('120×40')
  })
})

describe('TerminalSizeControl', () => {
  it('is disabled with the reason when the viewer cannot resize', () => {
    wrap(<TerminalSizeControl spaceId="spc_1" run={run()} blocker="Resizing needs terminal takeover permission" measure={() => null} observe={createRef()} />)
    expect(screen.getByTestId('terminal-size')).toBeDisabled()
  })

  it('applies a preset, fit and the node default, and never on its own', async () => {
    saveChoice({ kind: 'fixed', cols: 200, rows: 60 })
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: run({ terminal_size: { cols: 120, rows: 40 } }), response: new Response() } as never)
    const del = vi.spyOn(api, 'DELETE').mockResolvedValue({ data: run(), response: new Response() } as never)
    const r = run({ terminal_size: { cols: 100, rows: 30 } })
    wrap(<TerminalSizeControl spaceId="spc_1" run={r} blocker="" measure={() => ({ cols: 150, rows: 41 })} observe={createRef()} />)
    expect(put).not.toHaveBeenCalled() // the remembered 200x60 is only preselected
    fireEvent.click(screen.getByTestId('terminal-size'))
    await screen.findByTestId('terminal-size-120x40')
    fireEvent.click(screen.getByTestId('terminal-size-120x40'))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(1))
    expect(put.mock.calls[0][1]).toMatchObject({ params: { path: { space_id: 'spc_1', run_id: 'run_1' } }, body: { cols: 120, rows: 40 } })
    expect(choiceKey(loadChoice())).toBe('120x40')

    fireEvent.click(screen.getByTestId('terminal-size'))
    fireEvent.click(await screen.findByTestId('terminal-size-fit'))
    await waitFor(() => expect(put).toHaveBeenCalledTimes(2))
    expect(put.mock.calls[1][1]).toMatchObject({ body: { cols: 150, rows: 41 } })

    fireEvent.click(screen.getByTestId('terminal-size'))
    fireEvent.click(await screen.findByTestId('terminal-size-default'))
    await waitFor(() => expect(del).toHaveBeenCalledTimes(1))
    expect(loadChoice()).toEqual({ kind: 'default' })
  })
})
