import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type HarborPilotBudget, type HarborPilotStatus } from '@/api/client'
import { keys } from '@/api/queries'
import { HarborPilotLimitsCard } from './HarborPilotLimitsCard'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})

const SPACE = 'spc_1'

function budget(extra: Partial<HarborPilotBudget> = {}): HarborPilotBudget {
  return { budget_id: 'bdg_1', name: 'web daily', scope: 'project', scope_id: 'prj_web', period: 'day', period_start: '2026-11-08T00:00:00Z',
    period_end: '2026-11-09T00:00:00Z', currency: 'USD', enforcement: 'block', state: 'ok', limit: '10.00', spent: '2.50', reserved: '0.00',
    fraction: 0.25, blocking: false, ...extra } as HarborPilotBudget
}

function status(extra: Partial<HarborPilotStatus> = {}): HarborPilotStatus {
  return { space_id: SPACE, source: 'harbor-pilot', state: 'ok', connected: true, enabled: true, project_id: 'prj_web', harnesses: ['claude-code', 'codex'],
    on_budget_status_unavailable: 'proceed', budgets: [budget()], fetched_at: new Date().toISOString(),
    usage: { project_id: 'prj_web', clients: 'all', from: '2026-11-08T00:00:00Z', to: '2026-11-08T12:00:00Z', window: 'day', requests: 1234, input_tokens: 5000,
      output_tokens: 700, cache_read_tokens: 10, cache_write_tokens: 0, unknown_usage_requests: 2, costs: [{ kind: 'estimated_api_charge', currency: 'USD', amount: '2.50' }] },
    ...extra } as HarborPilotStatus
}

function wrap(s: HarborPilotStatus) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.spaceHarborPilotStatus(SPACE), s)
  return render(<MantineProvider><QueryClientProvider client={qc}><HarborPilotLimitsCard spaceId={SPACE} /></QueryClientProvider></MantineProvider>)
}

describe('Harbor Pilot Limits card', () => {
  it('shows nothing when the space does not route runs through Harbor Pilot', () => {
    const { container } = wrap(status({ state: 'off', enabled: false, budgets: [], usage: undefined }))
    expect(container.querySelector('[data-testid="harbor-pilot-limits"]')).toBeNull()
  })

  it('ok: hard limits labelled with their source, spend of limit, and project usage over all clients', () => {
    wrap(status())
    const card = screen.getByTestId('harbor-pilot-limits')
    expect(screen.getByTestId('harbor-pilot-state')).toHaveTextContent('Within budgets')
    expect(card).toHaveTextContent('Source: Harbor Pilot')
    expect(card).toHaveTextContent('Hard limits enforced by Harbor Pilot')
    expect(card).toHaveTextContent('Hard limit')
    expect(card).toHaveTextContent('2.50 of 10.00 USD')
    expect(card).toHaveTextContent('Project prj_web · per day')
    expect(screen.getByTestId('harbor-pilot-usage')).toHaveTextContent('Project usage, this day · all clients')
    expect(screen.getByTestId('harbor-pilot-usage')).toHaveTextContent('1,234')
    expect(screen.getByTestId('harbor-pilot-usage')).toHaveTextContent('2 without reported usage')
    expect(screen.getByTestId('harbor-pilot-usage')).toHaveTextContent('2.50 USD estimated')
    expect(card).not.toHaveTextContent('New runs are refused')
  })

  it('warning: a warn-only budget over its limit only warns', () => {
    wrap(status({ state: 'warning', budgets: [budget({ enforcement: 'warn', state: 'exceeded', spent: '12.00', fraction: 1.2 })] }))
    expect(screen.getByTestId('harbor-pilot-state')).toHaveTextContent('Warning')
    expect(screen.getByTestId('harbor-pilot-limits')).toHaveTextContent('Warn only')
    expect(screen.getByTestId('harbor-pilot-limits')).toHaveTextContent('Over (warn only)')
    expect(screen.getByTestId('harbor-pilot-limits')).not.toHaveTextContent('New runs are refused')
  })

  it('exceeded: a used-up blocking budget says launches are refused, with its spend and reset', () => {
    wrap(status({ state: 'exceeded', budgets: [budget({ state: 'exceeded', spent: '10.40', fraction: 1.04, blocking: true }), budget({ budget_id: 'bdg_g', name: 'all', scope: 'global', scope_id: '' })] }))
    expect(screen.getByTestId('harbor-pilot-state')).toHaveTextContent('Budget used up: launches refused')
    const card = screen.getByTestId('harbor-pilot-limits')
    expect(card).toHaveTextContent('New runs are refused')
    expect(card).toHaveTextContent('Budget “web daily” is used up (10.40 of 10.00 USD)')
    expect(card).toHaveTextContent('Used up')
    expect(card).toHaveTextContent('Every request')
    expect(screen.getAllByTestId('harbor-pilot-budget')).toHaveLength(2)
  })

  it('unavailable: says why and what launches do', () => {
    wrap(status({ state: 'unavailable', budgets: [], usage: undefined, fetched_at: undefined, unavailable: 'the Harbor Pilot connection has no service-principal token', on_budget_status_unavailable: 'refuse' }))
    expect(screen.getByTestId('harbor-pilot-state')).toHaveTextContent('Status unavailable')
    const card = screen.getByTestId('harbor-pilot-limits')
    expect(card).toHaveTextContent("Budget status can't be read")
    expect(card).toHaveTextContent('no service-principal token')
    expect(card).toHaveTextContent('This space refuses launches while budget status is unavailable.')
    expect(screen.queryAllByTestId('harbor-pilot-budget')).toHaveLength(0)
  })

  it('Harbor Pilot 0.1.4 budgets: one-time without an end, scheduled and ended ones render without a date of year 1', () => {
    wrap(status({ budgets: [
      budget({ budget_id: 'bdg_o', name: 'trial', period: 'once', period_end: undefined, state: 'ok', remaining: '7.50' }),
      budget({ budget_id: 'bdg_s', name: 'later', period: 'once', period_end: undefined, state: 'scheduled' }),
      budget({ budget_id: 'bdg_e', name: 'ended', period: 'once', period_end: '2026-10-31T00:00:00Z', state: 'expired', spent: '30.00', fraction: 3 }),
      budget({ budget_id: 'bdg_u', name: 'odd', state: 'unknown' }),
    ] }))
    const card = screen.getByTestId('harbor-pilot-limits')
    expect(screen.getByTestId('harbor-pilot-state')).toHaveTextContent('Within budgets')
    expect(card).not.toHaveTextContent('New runs are refused')
    expect(card).toHaveTextContent('Project prj_web · one time')
    expect(card).toHaveTextContent('7.50 USD left')
    expect(card).toHaveTextContent('Scheduled')
    expect(card).toHaveTextContent('Unknown state')
    const resets = screen.getAllByTestId('harbor-pilot-budget-reset').map((e) => e.textContent)
    expect(resets[0]).toBe('Never')
    expect(resets[1]).toBe('Never')
    expect(resets[2]).toMatch(/^Ended /)
    expect(card.textContent).not.toMatch(/\b1\/1\/1\b|0001|Invalid Date/)
  })

  it('a used-up one-time budget without an end says it never resets', () => {
    wrap(status({ state: 'exceeded', budgets: [budget({ name: 'trial', period: 'once', period_end: undefined, state: 'exceeded', spent: '12.00', fraction: 1.2, blocking: true })] }))
    const card = screen.getByTestId('harbor-pilot-limits')
    expect(card).toHaveTextContent('New runs are refused')
    expect(card).toHaveTextContent('one-time budget that never resets')
    expect(card.textContent).not.toMatch(/Invalid Date|0001/)
  })

  it('the budget bar is reachable from the keyboard and names the reserved amount', () => {
    wrap(status({ budgets: [budget({ reserved: '0.30' })] }))
    const bar = screen.getByTestId('harbor-pilot-budget-bar')
    expect(bar).toHaveAttribute('tabindex', '0')
    expect(bar.querySelector('[aria-label]')).toHaveAttribute('aria-label', '25 % used, 0.30 USD reserved by requests in flight')
  })

  it('error: a failed status read says so instead of showing nothing', async () => {
    vi.spyOn(api, 'GET').mockResolvedValue({ data: undefined, error: { title: 'Internal Server Error', status: 500, detail: 'status read failed' }, response: new Response(null, { status: 500 }) } as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<MantineProvider><QueryClientProvider client={qc}><HarborPilotLimitsCard spaceId={SPACE} /></QueryClientProvider></MantineProvider>)
    const card = await screen.findByTestId('harbor-pilot-limits-error')
    expect(card).toHaveTextContent("Harbor Pilot status can't be loaded")
    expect(card).toHaveTextContent('status read failed')
  })
})
