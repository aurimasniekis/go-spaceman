import { MantineProvider } from '@mantine/core'
import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { HumanAction } from '@/api/client'

vi.mock('@/api/queries', () => ({ useMe: () => ({ data: { root: true, user: { id: 'usr_me', display_name: 'Test Person' } } }) }))
vi.mock('@/api/session', () => ({ isMiniApp: () => true }))
vi.mock('@tanstack/react-query', async (orig) => ({ ...(await orig<object>()), useQueryClient: () => ({ invalidateQueries: () => {} }) }))
vi.mock('@/api/client', () => ({ api: { POST: () => Promise.resolve({}) }, ifMatch: () => '', unwrap: (r: unknown) => r }))
vi.mock('@/lib/notify', () => ({ notifyError: () => {}, notifyOk: () => {} }))
vi.mock('@/features/notifications/notify', () => ({ markOwnChange: () => {} }))
vi.mock('./ActionDetail', () => ({ ActionDetail: () => null }))

import { ActionList } from './ActionList'

const act = (id: string): HumanAction =>
  ({ id, space_id: 'spc_a', title: `Action ${id}`, priority: 'normal', created_at: '2026-11-01T00:00:00Z', type: 'question', status: 'open', version: 1 }) as unknown as HumanAction

const renderList = (noBulk?: boolean) => render(
  <MantineProvider>
    <ActionList actions={[act('a1'), act('a2')]} empty="Nothing" noBulk={noBulk} />
  </MantineProvider>,
)

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

describe('ActionList bulk operations', () => {
  it('offers select all, Claim and Acknowledge for a pending list by default', () => {
    renderList()
    expect(screen.getByRole('checkbox', { name: 'Select all' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Claim' })).toBeTruthy()
    expect(screen.getByRole('checkbox', { name: 'Select Action a1' })).toBeTruthy()
  })

  it('shows no bulk bar and no row selection with noBulk, but keeps the rows', () => {
    renderList(true)
    expect(screen.queryByRole('checkbox', { name: 'Select all' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Claim' })).toBeNull()
    expect(screen.queryByRole('checkbox', { name: 'Select Action a1' })).toBeNull()
    const rows = within(screen.getByRole('list', { name: 'Human actions' })).getAllByRole('listitem')
    expect(rows).toHaveLength(2)
  })
})
