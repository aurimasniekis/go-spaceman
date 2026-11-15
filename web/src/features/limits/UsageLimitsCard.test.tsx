import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me, type Run } from '@/api/client'
import { keys } from '@/api/queries'
import { RunState } from '@/components/StatusBadge'
import { axeViolations } from '@/test/axe'
import { theme } from '@/theme/theme'
import { RunLimitBadge } from './LimitTime'
import { UsageLimitsCard } from './UsageLimitsCard'
import { utcTime, type LimitWait, type UsageLimit } from './usageLimit'

const SPACE = 'spc_1'
const RESET = '2026-11-14T13:05:00Z'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  // Nothing reaches the network unless a test says so: every query is seeded.
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})
afterEach(() => vi.restoreAllMocks())

const operator = { root: false, method: 'password', user: { display_name: 'Op', grants: [{ role: 'operator', domain: SPACE }] } } as unknown as Me
const viewer = { root: false, method: 'password', user: { display_name: 'Viewer', grants: [{ role: 'viewer', domain: SPACE }] } } as unknown as Me
const admin = { root: false, method: 'password', user: { display_name: 'Admin', grants: [{ role: 'admin', domain: '*' }] } } as unknown as Me

function hold(extra: Partial<UsageLimit> = {}): UsageLimit {
  return { id: 'ulm_1', space_id: SPACE, scope: 'account', account_id: 'acc_1', kind: 'seven_day', source: 'transcript', resets_at: RESET,
    seen_at: '2026-11-14T09:00:00Z', first_run_id: 'run_1', version: 1, ...extra }
}
const shared: UsageLimit = { scope: 'account', account_id: 'acc_2', kind: 'five_hour', source: 'transcript', resets_at: '2026-11-14T11:00:00Z', seen_at: '2026-11-14T08:00:00Z', shared: true }

function wait(extra: Partial<LimitWait> = {}): LimitWait {
  return { id: 'lwt_1', space_id: SPACE, run_id: 'run_1', run_kind: 'worker', role: 'implementor', node_id: 'nod_1', instance_id: 'ins_1', harness: 'claude-code',
    resume_at: '2026-11-14T13:07:00Z', release_at: '2026-11-21T09:00:00Z', state: 'waiting', attempts: 0, version: 1, task_id: 'tsk_1',
    usage_limit_id: 'ulm_1', created_at: '2026-11-14T09:00:00Z', updated_at: '2026-11-14T09:00:00Z', ...extra }
}

function setup(me: Me, holds: UsageLimit[], waits: LimitWait[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, me)
  qc.setQueryData(keys.usageLimits(SPACE), holds)
  qc.setQueryData(keys.limitWaits(SPACE), waits)
  qc.setQueryData(keys.spaceAccounts(SPACE), [{ account_id: 'acc_1', name: 'team-max', harness: 'claude-code', enabled: true, default: true }])
  const router = createRouter({ routeTree: createRootRoute({ component: () => <UsageLimitsCard spaceId={SPACE} /> }), history: createMemoryHistory({ initialEntries: ['/'] }) })
  const view = render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  return { qc, view }
}

describe('Usage limits card', () => {
  it('lists held accounts and waiting runs with until when, local time shown and UTC kept for the tooltip', async () => {
    setup(operator, [hold()], [wait()])
    const h = await screen.findByTestId('usage-hold')
    expect(h).toHaveTextContent('team-max')
    expect(h).toHaveTextContent('weekly limit')
    expect(h).toHaveTextContent('Held until')
    // The time element carries the instant; its text is the viewer's zone and its tooltip says UTC.
    const t = h.querySelector(`time[datetime="${RESET}"]`) as HTMLElement
    expect(t).not.toBeNull()
    expect(t.textContent).toMatch(/\d\d:\d\d/)
    fireEvent.focus(t)
    expect(await screen.findByText(utcTime(RESET))).toBeInTheDocument()

    const w = screen.getByTestId('limit-wait')
    expect(w).toHaveTextContent('implementor')
    expect(w).toHaveTextContent('Waiting')
    expect(w).toHaveTextContent('Resumes at')
    expect(w).toHaveTextContent('Its task goes back to the queue at')
    expect(within(w).getByRole('link', { name: 'implementor' })).toHaveAttribute('href', `/spaces/${SPACE}/runs/run_1`)
    expect(within(w).getByRole('button', { name: 'Resume now' })).toBeEnabled()
    expect(within(w).getByRole('button', { name: 'Release' })).toBeEnabled()
    expect(within(h).getByRole('button', { name: 'Clear' })).toBeEnabled()
    expect(within(h).queryByRole('button', { name: 'Clear in every space' })).toBeNull()
    expect(await axeViolations()).toEqual([])
  })

  it("shows another space's hold as shared, without its details or a space-level clear", async () => {
    setup(admin, [shared], [])
    const h = await screen.findByTestId('usage-hold')
    expect(h).toHaveTextContent('shared · another space')
    expect(h).toHaveTextContent('5-hour limit')
    expect(h).not.toHaveTextContent('from the run transcript')
    expect(within(h).queryByRole('button', { name: 'Clear' })).toBeNull()
    // An admin of every account may still clear the account's holds everywhere.
    expect(within(h).getByRole('button', { name: 'Clear in every space' })).toBeEnabled()
    expect(await axeViolations()).toEqual([])
  })

  it("offers an operator who is not a global admin no action on another space's hold", async () => {
    setup(operator, [shared], [])
    const h = await screen.findByTestId('usage-hold')
    expect(h).toHaveTextContent('shared · another space')
    expect(within(h).queryByRole('button')).toBeNull()
  })

  it('names who cleared a hold in words, never the raw principal', async () => {
    setup(viewer, [hold({ id: 'ulm_old', cleared_at: '2026-11-14T08:00:00Z', cleared_by: 'user:usr_1' })], [])
    const c = await screen.findByTestId('usage-holds-cleared')
    expect(c).toHaveTextContent('(an operator)')
    expect(c).not.toHaveTextContent('usr_1')
  })

  it('offers viewers no actions, and says when nothing is held or waiting', async () => {
    setup(viewer, [hold({ id: 'ulm_old', cleared_at: '2026-11-14T08:00:00Z', cleared_by: 'run:run_9' })], [wait({ id: 'lwt_old', state: 'resumed', new_run_id: 'run_2' })])
    expect(await screen.findByTestId('usage-holds-empty')).toBeInTheDocument()
    expect(screen.getByTestId('limit-waits-empty')).toBeInTheDocument()
    expect(screen.getByTestId('usage-holds-cleared')).toHaveTextContent('a run reported it over')
    expect(screen.getByTestId('limit-waits-ended')).toHaveTextContent('implementor · resumed at')
    expect(screen.queryByRole('button')).toBeNull()
    expect(await axeViolations()).toEqual([])
  })

  it('an unknown reset says when it is checked again', async () => {
    setup(viewer, [hold({ resets_at: undefined, check_at: '2026-11-14T10:30:00Z', kind: 'unknown' })], [])
    const h = await screen.findByTestId('usage-hold')
    expect(h).toHaveTextContent('Reset time unknown; checked again at')
    expect(h.querySelector('time[datetime="2026-11-14T10:30:00Z"]')).not.toBeNull()
  })

  it('resume now posts and replaces the wait with the answer', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: wait({ state: 'resumed', new_run_id: 'run_2' }), response: new Response() } as never)
    const { qc } = setup(operator, [], [wait()])
    fireEvent.click(await screen.findByRole('button', { name: 'Resume now' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/limit-waits/{wait_id}/resume', { params: { path: { space_id: SPACE, wait_id: 'lwt_1' } } }))
    await waitFor(() => expect(qc.getQueryData<LimitWait[]>(keys.limitWaits(SPACE))?.[0].state).toBe('resumed'))
    expect(await screen.findByTestId('limit-waits-ended')).toHaveTextContent('resumed')
  })

  it('release asks first, then posts', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: wait({ state: 'released', last_error: 'operator' }), response: new Response() } as never)
    setup(operator, [], [wait()])
    fireEvent.click(await screen.findByRole('button', { name: 'Release' }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('Its task goes back to the queue, ready and unassigned')
    expect(post).not.toHaveBeenCalled()
    expect(await axeViolations()).toEqual([])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Release' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/limit-waits/{wait_id}/release', { params: { path: { space_id: SPACE, wait_id: 'lwt_1' } } }))
  })

  it('clear posts the space hold; clear in every space posts the account and kind', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: { cleared: [hold({ cleared_at: '2026-11-14T10:00:00Z', cleared_by: 'user:usr_1' })], held_elsewhere: true }, response: new Response() } as never)
    setup(admin, [hold()], [])
    fireEvent.click(await screen.findByRole('button', { name: 'Clear' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Clear' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/usage-limits/{limit_id}/clear', { params: { path: { space_id: SPACE, limit_id: 'ulm_1' } } }))
    expect(await screen.findByTestId('usage-holds-empty')).toBeInTheDocument()
  })

  it('clear in every space sends the hold kind to the account endpoint', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: { cleared: [], held_elsewhere: false }, response: new Response() } as never)
    setup(admin, [hold()], [])
    fireEvent.click(await screen.findByRole('button', { name: 'Clear in every space' }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent("Clears every space's weekly limit hold on team-max")
    fireEvent.click(within(dialog).getByRole('button', { name: 'Clear' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/accounts/{account_id}/usage-limits/clear', { params: { path: { account_id: 'acc_1' } }, body: { kind: 'seven_day' } }))
  })

  it('a refused action keeps the dialog open with the reason', async () => {
    vi.spyOn(api, 'POST').mockRejectedValue(new Error('the space is paused'))
    setup(operator, [], [wait()])
    fireEvent.click(await screen.findByRole('button', { name: 'Release' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Release' }))
    expect(await within(screen.getByRole('dialog')).findByRole('alert')).toHaveTextContent('the space is paused')
  })
})

function run(p: Partial<Run>): Run {
  return { id: 'run_1', role: 'implementor', kind: 'worker', harness: 'claude-code', observed: 'running', desired: 'running', status: 'idle', turn: 3, space_id: SPACE,
    created_at: '2026-11-14T08:00:00Z', started_at: '2026-11-14T08:00:00Z', created_by: 'user:usr_1', instance_id: 'ins_1', generation: 1, node_id: 'nod_1', launch: {}, ...p } as unknown as Run
}

describe('run limit badge', () => {
  const wrap = (ui: React.ReactNode) => render(<MantineProvider theme={theme}>{ui}</MantineProvider>)

  it('a live run at its limit shows the kind and the reset, UTC in the tooltip', async () => {
    wrap(<RunState run={run({ limit: { kind: 'five_hour', since: '2026-11-14T09:00:00Z', resets_at: RESET } })} />)
    const b = screen.getByTestId('run-limit-badge')
    expect(b).toHaveTextContent('5-hour limit · resets')
    const t = b.querySelector(`time[datetime="${RESET}"]`) as HTMLElement
    fireEvent.focus(t)
    expect(await screen.findByText(utcTime(RESET))).toBeInTheDocument()
    expect(await axeViolations()).toEqual([])
  })

  it('an unknown reset, a parked run, and a run with no limit', () => {
    const { rerender } = wrap(<RunLimitBadge run={run({ limit: { kind: 'unknown', since: '2026-11-14T09:00:00Z' } })} />)
    expect(screen.getByTestId('run-limit-badge')).toHaveTextContent('usage limit · resets at an unknown time')
    rerender(<MantineProvider theme={theme}><RunLimitBadge run={run({ observed: 'exited', end_reason: 'usage_limit' })} /></MantineProvider>)
    expect(screen.getByTestId('run-limit-badge')).toHaveTextContent('parked at usage limit')
    rerender(<MantineProvider theme={theme}><RunLimitBadge run={run({ observed: 'exited', end_reason: 'exited' })} /></MantineProvider>)
    expect(screen.queryByTestId('run-limit-badge')).toBeNull()
    rerender(<MantineProvider theme={theme}><RunLimitBadge run={run({})} /></MantineProvider>)
    expect(screen.queryByTestId('run-limit-badge')).toBeNull()
  })
})
