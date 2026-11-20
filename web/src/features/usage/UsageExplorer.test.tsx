import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { useState, type ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type GlobalUsageSeries, type UsageRow } from '@/api/client'
import { axeViolations } from '@/test/axe'
import { theme } from '@/theme/theme'
import { UsageExplorer } from './UsageExplorer'
import type { UsageSearch } from './state'

vi.mock('@/components/AnchorLink', () => ({ AnchorLink: ({ to, params, children }: { to: string; params: Record<string, string>; children: ReactNode }) => <a href={to.replace('$spaceId', params.spaceId)}>{children}</a> }))

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

const row = (day: number, group: string, tokens: number) => ({
  bucket_start: new Date(Date.UTC(2026, 10, day)).toISOString().replace('.000Z', 'Z'), group, total_tokens: tokens, input_tokens: tokens - 10, output_tokens: 10,
  cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, requests: 2, total_cost_usd: tokens / 1000,
}) as UsageRow

function page(items: UsageRow[], extra: Partial<GlobalUsageSeries> = {}): GlobalUsageSeries {
  const total = items.reduce((n, r) => n + r.total_tokens, 0)
  return { bucket: 'day', group_by: 'space', from: '2026-11-10T00:00:00Z', to: '2026-11-17T00:00:00Z', cost_note: 'List price, not a bill.', items, labels: {}, not_counted: [],
    totals: { group: '', total_tokens: total, input_tokens: total, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, requests: 2 * items.length, total_cost_usd: total / 1000 },
    ...extra } as GlobalUsageSeries
}

const windows = { items: [{ account_id: 'acc_a', name: 'team-max', harness: 'claude-code', windows: [{ kind: 'five_hour', used_pct: 64, observed_at: new Date().toISOString(), resets_at: new Date(Date.now() + 3600_000).toISOString() }], limits: [{ kind: 'seven_day', seen_at: new Date().toISOString() }] }] }

function Harness({ initial, seen }: { initial: UsageSearch; seen: UsageSearch[] }) {
  const [s, setS] = useState(initial)
  return <UsageExplorer search={s} onSearch={(n) => { seen.push(n); setS(n) }} />
}

function setup(get: (q: Record<string, unknown>) => GlobalUsageSeries, initial: UsageSearch = {}) {
  const seen: UsageSearch[] = []
  const queries: Record<string, unknown>[] = []
  vi.spyOn(api, 'GET').mockImplementation(((path: string, init?: { params?: { query?: Record<string, unknown> } }) => {
    if (path === '/usage/series') { queries.push(init?.params?.query ?? {}); return Promise.resolve({ data: get(init?.params?.query ?? {}), response: new Response() }) }
    if (path === '/account-windows') return Promise.resolve({ data: windows, response: new Response() })
    if (path === '/spaces') return Promise.resolve({ data: { items: [{ id: 'spc_1', name: 'Payments' }] }, response: new Response() })
    return new Promise(() => {})
  }) as never)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const view = render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><Harness initial={initial} seen={seen} /></QueryClientProvider></MantineProvider>)
  return { seen, queries, view }
}

describe('Usage explorer across spaces', () => {
  it('groups by space with labels from the response, links to the space page and has no axe violations', async () => {
    const { view } = setup(() => page([row(14, 'spc_1', 1200), row(15, 'spc_2', 300)], { labels: { spc_1: 'Payments', spc_2: 'Ledger' } }))
    const table = await screen.findByRole('table', { name: 'Usage by space' })
    expect(within(table).getByRole('link', { name: 'Payments' })).toHaveAttribute('href', '/spaces/spc_1/usage')
    expect(table).not.toHaveTextContent('spc_2')
    expect(table).toHaveTextContent('Ledger')
    expect(await axeViolations(view.container)).toEqual([])
  })
  it('asks for the chosen range and grouping, and writes them to the URL state', async () => {
    const { seen, queries } = setup(() => page([row(14, 'opus', 100)]))
    await screen.findByTestId('usage-totals')
    fireEvent.click(screen.getByRole('radio', { name: 'Last 90 days' }))
    fireEvent.click(screen.getByRole('radio', { name: 'Model' }))
    expect(seen.at(-1)).toEqual({ range: '90d', group: 'model' })
    expect(await screen.findByRole('table', { name: 'Usage by model' })).toBeInTheDocument()
    const last = queries.at(-1)!
    expect(last).toMatchObject({ bucket: 'day', group_by: 'model' })
    expect(Date.parse(last.to as string) - Date.parse(last.from as string)).toBe(90 * 86_400_000)
  })
  it('turns a clicked account into a filter chip that can be removed, named from the account list', async () => {
    const { seen, queries } = setup((q) => page([row(14, 'acc_a', 500)], { group_by: String(q.group_by) }), { group: 'account' })
    fireEvent.click(await screen.findByRole('button', { name: 'Filter by team-max' }))
    expect(seen.at(-1)).toEqual({ group: 'account', account: 'acc_a' })
    const chips = await screen.findByTestId('usage-filters')
    expect(chips).toHaveTextContent('Account: team-max')
    expect(queries.at(-1)).toMatchObject({ account_id: ['acc_a'] })
    fireEvent.click(within(chips).getByRole('button', { name: /Remove filter Account team-max/ }))
    expect(seen.at(-1)).toEqual({ group: 'account' })
  })
  it('names a space filter that came in through the URL', async () => {
    const { queries } = setup(() => page([row(14, 'opus', 100)]), { group: 'model', space: 'spc_1' })
    await waitFor(() => expect(screen.getByTestId('usage-filters')).toHaveTextContent('Space: Payments'))
    expect(queries[0]).toMatchObject({ space_id: ['spc_1'], group_by: 'model' })
  })
  it('lists not-counted runs per harness, only when there are some', async () => {
    setup(() => page([row(14, 'spc_1', 100)], { not_counted: [{ harness: 'codex', runs: 3, harbor_pilot_runs: 1 }, { harness: 'claude-code', runs: 0, harbor_pilot_runs: 0 }] }))
    const note = await screen.findByTestId('usage-not-counted')
    expect(note).toHaveTextContent('3 Codex runs (1 read back from Harbor Pilot)')
    expect(within(note).getAllByRole('listitem')).toHaveLength(1)
  })
  it('shows no note when nothing is uncounted, an empty state and the account windows', async () => {
    setup(() => page([]))
    expect(await screen.findByTestId('usage-empty')).toHaveTextContent('No usage was reported in this range.')
    expect(screen.queryByTestId('usage-not-counted')).toBeNull()
    const acc = await screen.findByTestId('usage-account-team-max')
    expect(acc).toHaveTextContent('64 % used')
    expect(acc).toHaveTextContent('7-day window')
    expect(screen.getByRole('progressbar', { name: 'team-max, 5-hour window' })).toHaveAttribute('aria-valuenow', '64')
  })
})

describe('Usage explorer follow-ups', () => {
  it('keeps the previous figures on screen while a new range loads', async () => {
    let release: (() => void) | undefined
    const seen: Record<string, unknown>[] = []
    vi.spyOn(api, 'GET').mockImplementation(((path: string, init?: { params?: { query?: Record<string, unknown> } }) => {
      if (path === '/usage/series') {
        seen.push(init?.params?.query ?? {})
        if (seen.length === 1) return Promise.resolve({ data: page([row(14, 'spc_1', 1200)], { labels: { spc_1: 'Payments' } }), response: new Response() })
        return new Promise((res) => { release = () => res({ data: page([row(14, 'spc_1', 5)]), response: new Response() }) })
      }
      if (path === '/account-windows') return Promise.resolve({ data: windows, response: new Response() })
      if (path === '/spaces') return Promise.resolve({ data: { items: [] }, response: new Response() })
      return new Promise(() => {})
    }) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><Harness initial={{}} seen={[]} /></QueryClientProvider></MantineProvider>)
    expect(await screen.findByRole('table', { name: 'Usage by space' })).toHaveTextContent('Payments')
    fireEvent.click(screen.getByRole('radio', { name: 'Last 90 days' }))
    await waitFor(() => expect(seen).toHaveLength(2))
    expect(screen.getByTestId('usage-totals')).toHaveTextContent('1,200')
    expect(screen.getByRole('table', { name: 'Usage by space' })).toBeInTheDocument()
    release!()
    await waitFor(() => expect(screen.getByTestId('usage-totals')).toHaveTextContent('5'))
  })
  it('uses the display name for a harness in the legend and the table', async () => {
    setup(() => page([row(14, 'claude-code', 700), row(15, 'codex', 300)], { group_by: 'harness' }), { group: 'harness' })
    const table = await screen.findByRole('table', { name: 'Usage by harness' })
    expect(table).toHaveTextContent('Claude Code')
    const legend = screen.getByRole('list', { name: 'Legend' })
    expect(legend).toHaveTextContent('Claude Code')
    expect(legend).toHaveTextContent('Codex')
    expect(legend).not.toHaveTextContent('claude-code')
  })
  it('names the filter toggle by what it does, and shows — for accounts filtered out', async () => {
    setup(() => page([row(14, 'acc_a', 500)], { group_by: 'account' }), { group: 'account' })
    const toggle = await screen.findByRole('button', { name: 'Filter by team-max' })
    fireEvent.click(toggle)
    expect(await screen.findByRole('button', { name: 'Remove filter team-max' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Filter by team-max' })).toBeNull()
    expect(await screen.findByRole('columnheader', { name: 'Tokens in range (filtered)' })).toBeInTheDocument()
    expect(screen.getByTestId('usage-account-team-max')).toHaveTextContent('500')
  })
  it('on a space page, shows the not-counted note from the space series with a link to the run pages', async () => {
    const calls: { path: string; space?: string }[] = []
    vi.spyOn(api, 'GET').mockImplementation(((path: string, init?: { params?: { path?: { space_id?: string } } }) => {
      calls.push({ path, space: init?.params?.path?.space_id })
      if (path === '/spaces/{space_id}/usage/series') return Promise.resolve({ data: page([row(14, 'worker', 100)], { not_counted: [{ harness: 'codex', runs: 2, harbor_pilot_runs: 0 }] }), response: new Response() })
      return new Promise(() => {})
    }) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><UsageExplorer spaceId="spc_1" search={{}} onSearch={() => {}} /></QueryClientProvider></MantineProvider>)
    const note = await screen.findByTestId('usage-not-counted')
    expect(note).toHaveTextContent('2 Codex runs')
    expect(within(note).getByRole('link', { name: 'run pages' })).toHaveAttribute('href', '/spaces/spc_1/runs')
    expect(calls).toContainEqual({ path: '/spaces/{space_id}/usage/series', space: 'spc_1' })
    expect(calls.map((c) => c.path)).not.toContain('/usage/series')
  })
  it('drops the old figures when the reload fails, and does not mark accounts against stale data', async () => {
    let n = 0
    vi.spyOn(api, 'GET').mockImplementation(((path: string) => {
      if (path === '/usage/series') {
        n++
        if (n === 1) return Promise.resolve({ data: page([row(14, 'spc_1', 1200)], { labels: { spc_1: 'Payments' } }), response: new Response() })
        return Promise.resolve({ data: undefined, error: { title: 'boom', status: 500 }, response: new Response(null, { status: 500 }) })
      }
      if (path === '/account-windows') return Promise.resolve({ data: windows, response: new Response() })
      if (path === '/spaces') return Promise.resolve({ data: { items: [] }, response: new Response() })
      return new Promise(() => {})
    }) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><Harness initial={{}} seen={[]} /></QueryClientProvider></MantineProvider>)
    await screen.findByTestId('usage-totals')
    fireEvent.click(screen.getByRole('radio', { name: 'Last 90 days' }))
    await waitFor(() => expect(screen.queryByTestId('usage-totals')).toBeNull())
    expect(screen.queryByRole('table', { name: 'Usage by space' })).toBeNull()
    expect(screen.getByRole('alert')).toBeInTheDocument()
  })
  it('hides the — account marks while the page is stale, and shows them again once it is fresh', async () => {
    let release: (() => void) | undefined
    let n = 0
    vi.spyOn(api, 'GET').mockImplementation(((path: string) => {
      if (path === '/usage/series') {
        n++
        if (n === 1) return Promise.resolve({ data: page([row(14, 'acc_other', 500)], { group_by: 'account' }), response: new Response() })
        return new Promise((res) => { release = () => res({ data: page([row(14, 'acc_other', 5)], { group_by: 'account' }), response: new Response() }) })
      }
      if (path === '/account-windows') return Promise.resolve({ data: windows, response: new Response() })
      if (path === '/spaces') return Promise.resolve({ data: { items: [] }, response: new Response() })
      return new Promise(() => {})
    }) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<MantineProvider theme={theme}><QueryClientProvider client={qc}><Harness initial={{ group: 'account', account: 'acc_other' }} seen={[]} /></QueryClientProvider></MantineProvider>)
    expect(await screen.findByTestId('usage-account-team-max')).toHaveTextContent('—')
    fireEvent.click(screen.getByRole('radio', { name: 'Last 90 days' }))
    await waitFor(() => expect(n).toBe(2))
    await waitFor(() => expect(document.querySelector('[data-stale="true"]')).not.toBeNull())
    expect(screen.getByTestId('usage-account-team-max')).not.toHaveTextContent('—')
    release!()
    await waitFor(() => expect(document.querySelector('[data-stale="true"]')).toBeNull())
    expect(await screen.findByTestId('usage-account-team-max')).toHaveTextContent('—')
  })
  it('shows — in the accounts table for an account the filter excludes', async () => {
    setup(() => page([row(14, 'acc_other', 500)], { group_by: 'account' }), { group: 'account', account: 'acc_other' })
    const acc = await screen.findByTestId('usage-account-team-max')
    expect(acc).toHaveTextContent('—')
    expect(acc).not.toHaveTextContent(/\b0\b/)
  })
})
