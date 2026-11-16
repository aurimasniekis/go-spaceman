import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type AccountSwitch, type Limits, type Me } from '@/api/client'
import { keys } from '@/api/queries'
import { axeViolations } from '@/test/axe'
import { theme } from '@/theme/theme'
import { AccountPoolsCard } from './AccountPoolsCard'
import { AccountSwitchesCard } from './AccountSwitchesCard'
import { AccountWindowsPanel } from './AccountWindowsPanel'
import { FailoverSettingsCard, overridesWith } from './FailoverSettingsCard'
import { RunSwitchNotice } from './RunSwitchNotice'

const SPACE = 'spc_1'
const admin = { root: false, method: 'password', user: { display_name: 'Admin', grants: [{ role: 'admin', domain: '*' }] } } as unknown as Me
const viewer = { root: false, method: 'password', user: { display_name: 'Viewer', grants: [{ role: 'viewer', domain: SPACE }] } } as unknown as Me

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})
afterEach(() => vi.restoreAllMocks())

const bindings = [
  { account_id: 'acc_a', name: 'team-max', harness: 'claude-code', enabled: true, default: true, priority: 10 },
  { account_id: 'acc_b', name: 'spare-pro', harness: 'claude-code', enabled: true, default: false, priority: 20 },
]

function limit(key: string, unit: string, over: Partial<Limits['limits'][number]> = {}) {
  return { key, label: key, unit, value: 0, default: 0, override: null, min: 0, max: 100, scope: 's', overridable: true, zero_means_none: false, ...over }
}
const limits = {
  version: 3, on_lower: 'drain',
  limits: [limit('usage_limit_failover', 'switch', { default: 1, value: 1 }), limit('usage_limit_wait_if_reset_within', 'minutes', { default: 15, value: 15 }),
    limit('usage_limit_switch_max_per_day', 'count', { default: 6, value: 6, override: 4 }), limit('usage_limit_min_headroom_percent', 'percent', { zero_means_none: true })],
} as unknown as Limits

function mount(me: Me, ui: ReactNode, seed: (qc: QueryClient) => void) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, me)
  qc.setQueryData(keys.spaceAccounts(SPACE), bindings)
  seed(qc)
  const router = createRouter({ routeTree: createRootRoute({ component: () => <>{ui}</> }), history: createMemoryHistory({ initialEntries: ['/'] }) })
  const view = render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  return { qc, view }
}

describe('pool order and role pools', () => {
  const seed = (qc: QueryClient) => {
    qc.setQueryData(keys.rolePools(SPACE), { accounts_version: 7, items: [{ role: 'implementor', items: [{ account_id: 'acc_b', name: 'spare-pro', priority: 10 }] }] })
    qc.setQueryData(keys.spaceRoles(SPACE), [{ name: 'implementor' }, { name: 'reviewer' }])
  }
  it('moves an account later and saves the order with the config version', async () => {
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: { items: [] }, response: new Response() } as never)
    mount(admin, <AccountPoolsCard spaceId={SPACE} />, seed)
    fireEvent.click(await screen.findByRole('button', { name: 'Move team-max later' }))
    fireEvent.click(screen.getByRole('button', { name: 'Save order' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    const [path, init] = put.mock.calls[0] as unknown as [string, { params: { header: Record<string, string> }; body: { items: { account_id: string; priority: number; default: boolean }[] } }]
    expect(path).toBe('/spaces/{space_id}/accounts')
    expect(init.params.header['If-Match']).toBe('"7"')
    expect(init.body.items).toEqual([{ account_id: 'acc_a', default: true, priority: 20 }, { account_id: 'acc_b', default: false, priority: 10 }])
  })
  it('edits a role pool and sends its order; a role without one says it uses the space accounts', async () => {
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: { items: [] }, response: new Response() } as never)
    mount(admin, <AccountPoolsCard spaceId={SPACE} />, seed)
    const impl = await screen.findByTestId('role-pool-implementor')
    expect(within(screen.getByTestId('role-pool-reviewer')).getByText("uses the space's accounts")).toBeInTheDocument()
    fireEvent.click(within(impl).getByRole('button', { name: 'Remove spare-pro from the pool of implementor' }))
    fireEvent.click(within(impl).getByRole('button', { name: 'Remove pool' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    const [path, init] = put.mock.calls[0] as unknown as [string, { params: { path: { role: string } }; body: { items: unknown[] } }]
    expect(path).toBe('/spaces/{space_id}/account-pools/{role}')
    expect(init.params.path.role).toBe('implementor')
    expect(init.body.items).toEqual([])
  })
  it('offers no editing to a viewer and has no axe violations', async () => {
    const { view } = mount(viewer, <AccountPoolsCard spaceId={SPACE} />, seed)
    await screen.findByTestId('role-pool-implementor')
    expect(screen.queryByRole('button', { name: /Move .* later/ })).toBeNull()
    expect(await axeViolations(view.container)).toEqual([])
  })
})

describe('failover settings', () => {
  it('keeps the space overrides it does not edit and clears a field left empty', () => {
    expect(overridesWith(limits.limits, { usage_limit_wait_if_reset_within: 30 })).toEqual({ usage_limit_switch_max_per_day: 4, usage_limit_wait_if_reset_within: 30 })
    expect(overridesWith(limits.limits, { usage_limit_switch_max_per_day: '' })).toEqual({})
  })
  it('saves the wait and headroom settings through the limits override', async () => {
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: limits, response: new Response() } as never)
    const { view } = mount(admin, <FailoverSettingsCard spaceId={SPACE} />, (qc) => qc.setQueryData(keys.limits(SPACE), limits))
    const wait = await screen.findByLabelText(/Wait instead of switching/)
    fireEvent.change(wait, { target: { value: '30' } })
    fireEvent.change(screen.getByLabelText(/Launch headroom/), { target: { value: '20' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    const [, init] = put.mock.calls[0] as unknown as [string, { params: { header: Record<string, string> }; body: { values: Record<string, number> } }]
    expect(init.params.header['If-Match']).toBe('"3"')
    expect(init.body.values).toEqual({ usage_limit_switch_max_per_day: 4, usage_limit_wait_if_reset_within: 30, usage_limit_min_headroom_percent: 20 })
    expect(await axeViolations(view.container)).toEqual([])
  })
})

describe('account windows', () => {
  it('shows each window with its percentage, reset and age, and flags an old observation', async () => {
    const recent = new Date(Date.now() - 5 * 60_000).toISOString()
    const { view } = mount(viewer, <AccountWindowsPanel spaceId={SPACE} />, (qc) => qc.setQueryData(keys.accountWindows(SPACE), [
      { account_id: 'acc_a', name: 'team-max', harness: 'claude-code', windows: [
        { kind: 'five_hour', used_pct: 42.4, resets_at: new Date(Date.now() + 3600_000).toISOString(), observed_at: recent },
        { kind: 'seven_day', used_pct: 91, resets_at: new Date(Date.now() + 86400_000).toISOString(), observed_at: '2020-01-01T00:00:00Z' }] },
      { account_id: 'acc_b', name: 'spare-pro', harness: 'claude-code', windows: [] },
    ]))
    const card = await screen.findByTestId('windows-team-max')
    expect(card).toHaveTextContent('5-hour window')
    expect(card).toHaveTextContent('42 % used')
    expect(card).toHaveTextContent('91 % used')
    expect(within(card).getAllByText('may be out of date')).toHaveLength(1)
    expect(screen.getByText(/No window reported yet for spare-pro/)).toBeInTheDocument()
    expect(await axeViolations(view.container)).toEqual([])
  })
})

describe('account switches', () => {
  const sw = (o: Partial<AccountSwitch>) => ({ id: 'asw_1', space_id: SPACE, role: 'implementor', from_run_id: 'run_a', from_account_id: 'acc_a', to_account_id: 'acc_b', limit_kind: 'five_hour', state: 'succeeded', new_run_id: 'run_b',
    created_at: '2026-11-14T09:00:00Z', updated_at: '2026-11-14T09:01:00Z', version: 1, ...o }) as AccountSwitch
  it('lists switches with account names, state, a fixed reason and run links', async () => {
    const { view } = mount(viewer, <AccountSwitchesCard spaceId={SPACE} />, (qc) => qc.setQueryData(keys.accountSwitches(SPACE),
      [sw({}), sw({ id: 'asw_2', state: 'fell_back', reason: 'no_eligible_account', to_account_id: '', new_run_id: '' })]))
    const card = await screen.findByTestId('account-switches')
    expect(card).toHaveTextContent('team-max → spare-pro')
    expect(card).toHaveTextContent('Moved')
    expect(card).toHaveTextContent('Waited for the reset instead')
    expect(card).toHaveTextContent('because no other account of the pool was free')
    expect(within(card).getAllByRole('link', { name: 'new run' })).toHaveLength(1)
    expect(card.textContent).not.toMatch(/acc_|asw_|run_/)
    expect(await axeViolations(view.container)).toEqual([])
  })
  it('says so when no run has switched', async () => {
    mount(viewer, <AccountSwitchesCard spaceId={SPACE} />, (qc) => qc.setQueryData(keys.accountSwitches(SPACE), []))
    expect(await screen.findByText('No run has switched accounts yet.')).toBeInTheDocument()
  })
  it('links a run to the switch away from it and the one into it', async () => {
    mount(viewer, <RunSwitchNotice spaceId={SPACE} runId="run_b" />, (qc) => qc.setQueryData(keys.accountSwitches(SPACE),
      [sw({ id: 'asw_2', from_run_id: 'run_b', new_run_id: 'run_c', from_account_id: 'acc_b', to_account_id: 'acc_a' }), sw({})]))
    const n = await screen.findByTestId('run-switch')
    expect(n).toHaveTextContent('Moved to account team-max')
    expect(n).toHaveTextContent('Continued from the run on account team-max')
    expect(within(n).getAllByRole('link')).toHaveLength(2)
  })
  it('renders nothing for an untouched run', async () => {
    mount(viewer, <RunSwitchNotice spaceId={SPACE} runId="run_z" />, (qc) => qc.setQueryData(keys.accountSwitches(SPACE), [sw({})]))
    await waitFor(() => expect(screen.queryByTestId('run-switch')).toBeNull())
  })
})
