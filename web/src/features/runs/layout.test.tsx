import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me, type Run, type Space } from '@/api/client'
import { keys, runsQuery } from '@/api/queries'
import { SpaceHeader } from '@/features/spaces/SpaceHeader'
import { NARROW_QUERY } from '@/lib/media'
import { RunWorkbench } from './RunWorkbench'
import { SpaceTerminals } from './SpaceTerminals'
import { Workspace } from './Workspace'

// ghostty-web needs WebAssembly and a canvas; these tests are about the layout around the terminal.
vi.mock('@/features/terminal/TerminalView', () => ({ TerminalView: () => <div data-testid="terminal" className="sm-terminal" /> }))

const SPACE = 'spc_1'
let narrow = false

beforeEach(() => {
  narrow = false
  // jsdom has no matchMedia: the phone layout is "matches" for the narrow query.
  window.matchMedia = ((q: string) => ({
    matches: narrow && q === NARROW_QUERY, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  // Nothing may reach the network: every query that matters is seeded, the rest stays pending.
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
  vi.spyOn(api, 'POST').mockImplementation(() => new Promise(() => {}))
})
afterEach(() => vi.restoreAllMocks())

function run(id: string, p: Partial<Run> = {}): Run {
  return {
    id, role: 'reviewer', kind: 'worker', harness: 'claude-code', observed: 'running', desired: 'running', status: 'working', turn: 11, space_id: SPACE,
    created_at: '2026-11-07T08:00:00Z', started_at: '2026-11-07T08:00:00Z', created_by: 'user:usr_1', instance_id: 'ins_1', generation: 1, node_id: 'nod_1',
    launch: {}, ...p,
  } as unknown as Run
}

const me = { root: true, method: 'password', user: { display_name: 'Op', grants: [] } } as unknown as Me
const runs = [run('run_control', { role: 'control', kind: 'control' }), ...Array.from({ length: 8 }, (_, i) => run(`run_w${i}`, { role: `worker-${i}` }))]
const spaces = [{ id: SPACE, name: 'SpaceMan', slug: 'spaceman', state: 'active', node_id: 'nod_1' }] as unknown as Space[]

/** Renders inside Mantine, a seeded query client and a router (links in the inspector and the workspace need one). */
async function show(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, me)
  qc.setQueryData(keys.spaces, spaces)
  qc.setQueryData(keys.space(SPACE), spaces[0])
  qc.setQueryData(keys.nodes, [])
  qc.setQueryData(runsQuery(SPACE).queryKey, runs)
  for (const r of runs) qc.setQueryData(keys.run(SPACE, r.id), r)
  const router = createRouter({ routeTree: createRootRoute({ component: () => <>{ui}</> }), history: createMemoryHistory({ initialEntries: ['/'] }) })
  const view = render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  await screen.findAllByRole('button')
  return view
}

describe('agent tab strip', () => {
  it('scrolls sideways only, with no scroll area and tabs sized to their content', async () => {
    await show(<SpaceTerminals spaceId={SPACE} runId="run_control" onSelect={() => {}} onMode={() => {}} />)
    const strip = screen.getByRole('tablist', { name: 'Agents' })
    expect(strip).toHaveClass('sm-tabstrip')
    expect(strip.style.overflowY).toBe('hidden')
    expect(strip.style.overflowX).toBe('auto')
    expect(strip.style.flexWrap).toBe('nowrap')
    // Mantine's ScrollArea drew both scrollbars and let the two-line tabs scroll vertically inside it.
    expect(strip.closest('.mantine-ScrollArea-root')).toBeNull()
    const tabs = within(strip).getAllByRole('tab')
    expect(tabs).toHaveLength(runs.length)
    for (const t of tabs) {
      expect(t.style.flex).toBe('0 0 auto')
      expect(t.style.minWidth).toBe('')
      // A negative margin pushed the tab 1 px past the strip, which made it scroll vertically.
      expect(t.style.marginBottom).toBe('')
    }
  })

  it('turns a vertical mouse wheel into sideways scrolling when tabs overflow', async () => {
    await show(<SpaceTerminals spaceId={SPACE} runId="run_control" onSelect={() => {}} onMode={() => {}} />)
    const strip = screen.getByRole('tablist', { name: 'Agents' })
    Object.defineProperty(strip, 'scrollWidth', { value: 1200, configurable: true })
    Object.defineProperty(strip, 'clientWidth', { value: 400, configurable: true })
    const ev = new WheelEvent('wheel', { deltaY: 120, cancelable: true })
    strip.dispatchEvent(ev)
    expect(strip.scrollLeft).toBe(120)
    expect(ev.defaultPrevented).toBe(true)
  })
})

describe('run header', () => {
  const r = run('run_w0', { role: 'reviewer-with-a-long-role-name' })

  it('keeps the turn and the state whole on a desktop and truncates the role first', async () => {
    await show(<RunWorkbench spaceId={SPACE} run={r} mode="live" onMode={() => {}} inspectorDefault={false} />)
    const header = screen.getByTestId('run-header')
    expect(header.style.getPropertyValue('--group-wrap')).toBe('wrap')
    expect(screen.getByTestId('run-header-turn')).toHaveTextContent('#11')
    expect(screen.getByTestId('run-header-turn').style.flexShrink).toBe('0')
    expect(screen.getByTestId('run-header-state').style.flexShrink).toBe('0')
    expect(screen.getByTestId('run-header-state').style.whiteSpace).toBe('nowrap')
    const role = screen.getByTestId('run-header-role')
    expect(Number(role.style.flexShrink)).toBeGreaterThan(1)
    expect(role.style.textOverflow).toBe('ellipsis')
    expect(screen.getByTestId('run-header-who').style.minWidth).toBe('0px')
    expect(screen.getByTestId('run-header-actions').style.flexShrink).toBe('0')
    // Full-text buttons on a desktop.
    expect(screen.getByRole('button', { name: /Take over/ })).toHaveTextContent('Take over')
    expect(screen.getByRole('button', { name: /Stop/ })).toHaveTextContent('Stop')
  })

  it('puts the actions on their own row of icon buttons on a phone', async () => {
    narrow = true
    await show(<RunWorkbench spaceId={SPACE} run={r} mode="live" onMode={() => {}} inspectorDefault />)
    const actions = screen.getByTestId('run-header-actions')
    expect(actions.style.flexBasis).toBe('100%')
    const takeover = within(actions).getByRole('button', { name: 'Take over' })
    const stop = within(actions).getByRole('button', { name: 'Stop' })
    expect(takeover).toHaveTextContent('')
    expect(stop).toHaveTextContent('')
    expect(within(actions).getByRole('radiogroup', { name: 'Terminal view' })).toBeInTheDocument()
    // The kind and harness give way on a phone; the role, turn and state stay on the first row.
    expect(screen.getByTestId('run-header-who')).not.toHaveTextContent('Worker')
    expect(screen.getByTestId('run-header-turn')).toBeInTheDocument()
    // The inspector stacks under the terminal instead of taking 300 px beside it.
    expect(screen.getByTestId('run-body').style.gridTemplateColumns).toBe('minmax(0, 1fr)')
  })

  it('keeps the inspector beside the terminal on a desktop', async () => {
    await show(<RunWorkbench spaceId={SPACE} run={r} mode="live" onMode={() => {}} inspectorDefault />)
    expect(screen.getByTestId('run-body').style.gridTemplateColumns).toBe('minmax(0, 1fr) 300px')
  })
})

describe('terminals workspace', () => {
  it('shows the space list beside the terminals on a desktop', async () => {
    await show(<Workspace spaceId={SPACE} runId="run_control" onSelect={() => {}} onMode={() => {}} />)
    expect(screen.getByRole('textbox', { name: 'Filter spaces and agents' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Hide the space list' })).toBeInTheDocument()
  })

  it('puts the space list in a drawer on a phone, opened from the space name', async () => {
    narrow = true
    const onSelect = vi.fn()
    await show(<Workspace spaceId={SPACE} runId="run_control" onSelect={onSelect} onMode={() => {}} />)
    // No sidebar column and no second sidebar toggle: one button names the space and opens the list.
    expect(screen.queryByRole('textbox', { name: 'Filter spaces and agents' })).toBeNull()
    expect(screen.queryByRole('button', { name: /space list/ })).toBeNull()
    const open = screen.getByRole('button', { name: /Choose a space or agent/ })
    expect(open).toHaveTextContent('SpaceMan')
    fireEvent.click(open)
    const drawer = await screen.findByRole('dialog', { name: 'Terminals' })
    expect(within(drawer).getByRole('textbox', { name: 'Filter spaces and agents' })).toBeInTheDocument()
    fireEvent.click(within(drawer).getByText(/^worker-3/))
    expect(onSelect).toHaveBeenCalledWith(SPACE, 'run_w3')
  })
})

describe('space header', () => {
  it('wraps its actions under the name instead of squeezing the name', async () => {
    await show(<SpaceHeader spaceId={SPACE} />)
    const header = screen.getByTestId('space-header')
    expect(header.style.getPropertyValue('--group-wrap')).toBe('wrap')
    expect(screen.getByRole('heading', { name: 'SpaceMan' }).closest('.mantine-Stack-root')).toHaveStyle({ minWidth: '0px' })
    expect(screen.getByRole('button', { name: /Launch run/ }).closest('.mantine-Group-root')).toHaveStyle({ flexShrink: '0' })
  })
})
