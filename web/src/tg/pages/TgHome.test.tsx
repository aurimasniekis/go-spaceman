import { MantineProvider } from '@mantine/core'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { HumanAction } from '@/api/client'

const state = vi.hoisted(() => ({ pending: undefined as unknown }))
vi.mock('@/api/queries', () => ({
  useMe: () => ({ data: { user: { display_name: 'Test Person' } } }),
  useSpaces: () => ({ data: [{ id: 'spc_a', name: 'Alpha', state: 'active' }, { id: 'spc_b', name: 'Beta', state: 'active' }], isSuccess: true, error: null }),
  useAttention: () => ({ data: { total: 7 } }),
  useHumanPending: () => ({ data: state.pending, isSuccess: state.pending !== undefined, error: null }),
  runsQuery: (id: string) => ({ queryKey: ['runs', id], queryFn: () => [] }),
}))
vi.mock('@tanstack/react-query', async (orig) => ({ ...(await orig<object>()), useQueries: () => [] }))
vi.mock('@tanstack/react-router', () => ({ Link: ({ to, children, ...p }: { to: string; children: React.ReactNode }) => <a href={to} {...p}>{children}</a> }))
vi.mock('@/components/AnchorLink', () => ({ AnchorLink: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a> }))
vi.mock('@/components/StatusBadge', () => ({ SpaceState: () => null }))
// The list itself is ActionList's job (its own tests cover it); here it shows which rows the home picked, and in what order.
vi.mock('@/features/human/ActionList', () => ({
  ActionList: ({ actions, spaceNames }: { actions: HumanAction[]; spaceNames?: Record<string, string> }) => (
    <ul aria-label="Human actions">{actions.map((a) => <li key={a.id}>{a.title} · {spaceNames?.[a.space_id]}</li>)}</ul>
  ),
}))

import { TgHome } from './TgHome'

const act = (id: string, priority: HumanAction['priority'], created: string, space = 'spc_a'): HumanAction =>
  ({ id, space_id: space, title: `Action ${id}`, priority, created_at: created, type: 'question', status: 'open' }) as HumanAction

const show = () => render(<MantineProvider><TgHome /></MantineProvider>)
const rows = () => within(screen.getByRole('list', { name: 'Human actions' })).getAllByRole('listitem').map((li) => li.textContent)

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

describe('TgHome Needs you', () => {
  it('shows the cross-space total and the most urgent, then oldest, five with their space names', () => {
    state.pending = {
      total: 7,
      counts: { spc_a: 4, spc_b: 3 },
      items: [
        act('n1', 'normal', '2026-11-01T00:00:00Z'),
        act('n2', 'normal', '2026-11-02T00:00:00Z', 'spc_b'),
        act('n3', 'normal', '2026-11-03T00:00:00Z'),
        act('n4', 'normal', '2026-11-04T00:00:00Z'),
        act('n5', 'normal', '2026-11-05T00:00:00Z', 'spc_b'),
        act('u1', 'urgent', '2026-11-14T00:00:00Z', 'spc_b'),
        act('h1', 'high', '2026-11-13T00:00:00Z'),
      ],
    }
    show()
    expect(screen.getByRole('heading', { name: 'Needs you' })).toBeTruthy()
    expect(screen.getByText('7')).toBeTruthy()
    expect(rows()).toEqual([
      'Action u1 · Beta',
      'Action h1 · Alpha',
      'Action n1 · Alpha',
      'Action n2 · Beta',
      'Action n3 · Alpha',
    ])
  })

  it('links See all to /human and keeps Needs attention as a secondary line', () => {
    state.pending = { total: 0, counts: {}, items: [] }
    show()
    expect(screen.getByRole('link', { name: 'See all' }).getAttribute('href')).toBe('/human')
    expect(screen.getByText('Nothing is waiting for you.')).toBeTruthy()
    expect(screen.getByText(/Needs attention 7/)).toBeTruthy()
    expect(screen.getByRole('link', { name: 'Open' }).getAttribute('href')).toBe('/attention')
  })

  it('shows the count as it changes', () => {
    state.pending = { total: 2, counts: {}, items: [act('a', 'normal', '2026-11-01T00:00:00Z')] }
    const view = show()
    expect(screen.getByText('2')).toBeTruthy()
    state.pending = { total: 3, counts: {}, items: [act('a', 'normal', '2026-11-01T00:00:00Z'), act('b', 'normal', '2026-11-02T00:00:00Z')] }
    view.rerender(<MantineProvider><TgHome /></MantineProvider>)
    expect(screen.getByText('3')).toBeTruthy()
    expect(rows()).toHaveLength(2)
  })
})
