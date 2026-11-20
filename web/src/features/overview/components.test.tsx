import { MantineProvider } from '@mantine/core'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import type { AccountState, AttentionItem, GlobalUsageSeries, UsageRow } from '@/api/client'
import { axeViolations } from '@/test/axe'
import { theme } from '@/theme/theme'
import { AccountWindowsStrip } from './AccountWindowsStrip'
import { AttentionTop } from './AttentionTop'
import { overviewRange } from './summary'
import { UsageSummary } from './UsageSummary'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

async function view(ui: ReactNode) {
  const router = createRouter({ routeTree: createRootRoute({ component: () => <MantineProvider theme={theme}>{ui}</MantineProvider> }), history: createMemoryHistory() })
  await router.load()
  return render(<RouterProvider router={router} />)
}

const item = (i: number, severity: 'act' | 'watch'): AttentionItem => ({
  id: `it${i}`, kind: 'task_failed', severity, since: `2026-11-20T0${i}:00:00Z`, space_id: 'spc_1', title: `Item ${i}`, target: { type: 'task', space_id: 'spc_1' },
})

describe('AttentionTop', () => {
  it('lists at most five items, act first, with See all (N)', async () => {
    const items = [1, 2, 3, 4, 5, 6, 7].map((i) => item(i, i === 7 ? 'watch' : 'act'))
    const { container } = await view(<AttentionTop items={items} total={9} spaceName={() => 'Payments'} />)
    expect(screen.getAllByRole('listitem')).toHaveLength(5)
    expect(screen.queryByText('Item 7')).toBeNull()
    expect(screen.getByRole('link', { name: 'See all (9)' }).getAttribute('href')).toBe('/attention')
    expect(screen.getByRole('link', { name: 'Item 6' }).getAttribute('href')).toBe('/spaces/spc_1/queue')
    expect(await axeViolations(container)).toEqual([])
  })
  it('shows loading and error states, never "all clear" or a count of 0', async () => {
    const { unmount } = await view(<AttentionTop loading spaceName={() => ''} />)
    expect(screen.getByText('Loading…')).toBeTruthy()
    expect(screen.queryByText('Nothing needs you right now.')).toBeNull()
    expect(screen.getByRole('link', { name: 'See all' })).toBeTruthy()
    unmount()
    await view(<AttentionTop error={new Error('boom')} spaceName={() => ''} />)
    expect(screen.getByText('boom')).toBeTruthy()
    expect(screen.queryByText('Nothing needs you right now.')).toBeNull()
    expect(screen.queryByRole('link', { name: /See all \(/ })).toBeNull()
  })
  it('says so when nothing needs attention', async () => {
    await view(<AttentionTop items={[]} total={0} spaceName={() => ''} />)
    expect(screen.getByText('Nothing needs you right now.')).toBeTruthy()
  })
})

const NOW = Date.UTC(2026, 10, 20, 15)
const { from, to } = overviewRange(NOW)
const row = (d: number, group: string, tokens: number) => ({ bucket_start: new Date(Date.parse(from) + d * 86_400_000).toISOString().replace('.000Z', 'Z'), group, total_tokens: tokens }) as UsageRow
const series = (items: UsageRow[], labels: Record<string, string> = {}) => ({ items, labels, from, to, bucket: 'day' }) as unknown as GlobalUsageSeries

describe('UsageSummary', () => {
  it('shows tokens today and for 7 days, the delta, and the top lists, without a cost', async () => {
    const bySpace = series([row(13, 'spc_1', 1500), row(9, 'spc_2', 500), row(2, 'spc_1', 1000)], { spc_1: 'Payments', spc_2: 'Search' })
    const byModel = series([row(13, 'claude-x', 2000)])
    const { container } = await view(<UsageSummary bySpace={bySpace} byModel={byModel} from={from} />)
    expect(screen.getAllByText('1.5K').length).toBeGreaterThan(0)
    expect(screen.getAllByText('2K').length).toBeGreaterThan(0)
    expect(screen.getByTestId('usage-delta').textContent).toContain('100 %')
    expect(screen.getByText('Payments')).toBeTruthy()
    expect(screen.getByText('claude-x')).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Open Usage' }).getAttribute('href')).toBe('/usage')
    expect(container.textContent).not.toContain('$')
    expect(await axeViolations(container)).toEqual([])
  })
  it('has no delta without an earlier period', async () => {
    await view(<UsageSummary bySpace={series([row(13, 'spc_1', 10)])} byModel={series([])} from={from} />)
    expect(screen.getByTestId('usage-delta').textContent).toContain('no earlier period')
  })
})

describe('AccountWindowsStrip', () => {
  const acc = (over: Partial<AccountState>): AccountState => ({ account_id: 'a', name: 'team-max', limits: [], windows: [], ...over })
  it('renders nothing without accounts', async () => {
    const { container } = await view(<AccountWindowsStrip accounts={[]} now={NOW} />)
    expect(container.querySelector('[data-testid="account-windows-strip"]')).toBeNull()
  })
  it('shows both window bars, an open limit and a stale mark', async () => {
    const fresh = new Date(NOW - 60_000).toISOString()
    const old = new Date(NOW - 3 * 3600_000).toISOString()
    const accounts = [
      acc({ windows: [{ kind: 'five_hour', used_pct: 42.4, observed_at: fresh }, { kind: 'seven_day', used_pct: 95, observed_at: old }], limits: [{ kind: 'five_hour', seen_at: fresh }] }),
      acc({ account_id: 'b', name: 'idle', windows: [] }),
    ]
    const { container } = await view(<AccountWindowsStrip accounts={accounts} now={NOW} />)
    expect(screen.getByRole('progressbar', { name: 'team-max, 5 h window' }).getAttribute('aria-valuenow')).toBe('42')
    expect(screen.getByRole('progressbar', { name: 'team-max, 7 d window, may be out of date' }).getAttribute('aria-valuenow')).toBe('95')
    expect(screen.getByText('at a limit')).toBeTruthy()
    expect(screen.getByText('No window reported yet')).toBeTruthy()
    expect(await axeViolations(container)).toEqual([])
  })
})

describe('UsageSummary states', () => {
  it('shows a placeholder while loading and the error on failure, no figures', async () => {
    const r = overviewRange()
    const { unmount } = await view(<UsageSummary from={r.from} loading />)
    expect(screen.getByText('Loading…')).toBeTruthy()
    for (const t of ['Today', 'Last 7 days', 'No usage', 'no earlier period to compare']) expect(screen.queryByText(t)).toBeNull()
    unmount()
    await view(<UsageSummary from={r.from} error={new Error('usage broke')} />)
    expect(screen.getByText('usage broke')).toBeTruthy()
    expect(screen.queryByText('No usage')).toBeNull()
  })
})
