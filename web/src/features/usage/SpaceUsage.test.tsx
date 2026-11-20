import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type UsageRow, type UsageSeries } from '@/api/client'
import { keys } from '@/api/queries'
import { axeViolations } from '@/test/axe'
import { theme } from '@/theme/theme'
import { SpaceUsage } from './SpaceUsage'

const SPACE = 'spc_1'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

const row = (day: number, group: string, tokens: number, cost: number) => ({
  bucket_start: new Date(Date.UTC(2026, 10, day)).toISOString().replace('.000Z', 'Z'), group, total_tokens: tokens, input_tokens: tokens - 10, output_tokens: 10,
  cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, requests: 3, total_cost_usd: cost,
}) as UsageRow

function page(items: UsageRow[], extra: Partial<UsageSeries> = {}): UsageSeries {
  const sum = (k: 'total_tokens' | 'input_tokens' | 'output_tokens' | 'total_cost_usd') => items.reduce((n, r) => n + r[k], 0)
  return { bucket: 'day', group_by: 'role', from: '2026-11-10T00:00:00Z', to: '2026-11-17T00:00:00Z', cost_note: 'The cost is the harness list-price estimate, not a bill.', items,
    totals: { group: '', total_tokens: sum('total_tokens'), input_tokens: sum('input_tokens'), output_tokens: sum('output_tokens'), cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, requests: 3 * items.length, total_cost_usd: sum('total_cost_usd') },
    ...extra } as UsageSeries
}

function setup(get: (query: Record<string, unknown>) => UsageSeries) {
  const spy = vi.spyOn(api, 'GET').mockImplementation(((path: string, init?: { params?: { query?: Record<string, unknown> } }) => {
    if (path === '/spaces/{space_id}/usage/series') return Promise.resolve({ data: get(init?.params?.query ?? {}), response: new Response() })
    if (path === '/usage/series') return Promise.resolve({ data: { not_counted: [] }, response: new Response() })
    return new Promise(() => {})
  }) as never)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(keys.spaceAccounts(SPACE), [{ account_id: 'acc_a', name: 'team-max', harness: 'claude-code', enabled: true, default: true }])
  const view = render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><SpaceUsage spaceId={SPACE} /></QueryClientProvider></MantineProvider>)
  return { spy, view }
}

describe('Space usage', () => {
  it('shows tokens first, the cost as a labelled estimate, a table of the same numbers and no axe violations', async () => {
    const { view } = setup(() => page([row(14, 'worker', 1200, 0.5), row(15, 'worker', 800, 0.25), row(15, 'control', 300, 0.1)]))
    const totals = await screen.findByTestId('usage-totals')
    expect(totals).toHaveTextContent('2,300')
    expect(totals).toHaveTextContent('Estimated cost')
    expect(totals).toHaveTextContent('≈ $0.850')
    expect(totals).toHaveTextContent('list price, not a bill')
    expect(screen.getByText('The cost is the harness list-price estimate, not a bill.')).toBeInTheDocument()
    const table = screen.getByRole('table', { name: 'Usage by role' })
    expect(within(table).getByText('worker').closest('tr')).toHaveTextContent('2,000')
    expect(screen.getByRole('list', { name: 'Legend' })).toHaveTextContent('control')
    expect(screen.getAllByRole('img').some((b) => (b.getAttribute('aria-label') ?? '').includes('Nov 15: 1,100 tokens. control 300, worker 800'))).toBe(true)
    expect(await axeViolations(view.container)).toEqual([])
  })
  it('shows a tooltip for the focused bar', async () => {
    setup(() => page([row(14, 'worker', 1200, 0.5)]))
    const bar = (await screen.findAllByRole('img')).find((b) => (b.getAttribute('aria-label') ?? '').startsWith('Nov 14'))!
    fireEvent.focus(bar)
    expect(screen.getByTestId('usage-tooltip')).toHaveTextContent('1,200 tokens')
  })
  it('groups by account with the account names and asks the API for the chosen grouping', async () => {
    const { spy } = setup((q) => page([row(14, q.group_by === 'account' ? 'acc_a' : 'worker', 100, 0.01)], { group_by: String(q.group_by) } as Partial<UsageSeries>))
    await screen.findByTestId('usage-totals')
    fireEvent.click(screen.getByRole('radio', { name: 'Account' }))
    expect(await screen.findByRole('table', { name: 'Usage by account' })).toHaveTextContent('team-max')
    expect(spy.mock.calls.some((c) => (c[1] as { params?: { query?: { group_by?: string } } } | undefined)?.params?.query?.group_by === 'account')).toBe(true)
  })
  it('follows the cursor to the next page', async () => {
    const { spy } = setup((q) => (q.cursor ? page([row(15, 'worker', 50, 0)]) : page([row(14, 'worker', 100, 0)], { next_cursor: 'c1' })))
    await waitFor(() => expect(spy.mock.calls.filter((c) => c[0] === '/spaces/{space_id}/usage/series')).toHaveLength(2))
    expect((await screen.findByRole('table', { name: 'Usage by role' }))).toHaveTextContent('150')
  })
  it('says so when nothing was reported', async () => {
    setup(() => page([]))
    expect(await screen.findByTestId('usage-empty')).toHaveTextContent('No usage was reported in this range.')
  })
})
