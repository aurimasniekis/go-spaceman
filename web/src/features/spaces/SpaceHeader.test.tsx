import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me, type Space } from '@/api/client'
import { keys } from '@/api/queries'
import { SpaceHeader } from './SpaceHeader'

const space = { id: 'spc_1', slug: 'payments', name: 'Payments', state: 'active', node_id: 'nod_1', version: 1 } as unknown as Space

const me = (grants: { role: string; domain: string }[]): Me => ({ method: 'password', user: { id: 'usr_1', grants } }) as unknown as Me

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  // Everything the header needs is seeded in the cache; anything else stays pending.
  vi.spyOn(api, 'GET').mockImplementation((() => new Promise(() => {})) as never)
})

afterEach(() => vi.restoreAllMocks())

/** The header inside a route tree shaped like the app's: the space layout with its overview and inbox pages. */
async function show(who: Me, path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, who)
  qc.setQueryData(keys.space(space.id), space)
  qc.setQueryData(keys.nodes, [])
  const root = createRootRoute({ component: () => <Outlet /> })
  const layout = createRoute({ getParentRoute: () => root, path: 'spaces/$spaceId', component: () => <SpaceHeader spaceId={space.id} /> })
  const overview = createRoute({ getParentRoute: () => layout, path: '/' })
  const inbox = createRoute({ getParentRoute: () => layout, path: 'inbox' })
  const router = createRouter({ routeTree: root.addChildren([layout.addChildren([overview, inbox])]), history: createMemoryHistory({ initialEntries: [path] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  await screen.findByText('Payments')
}

const newItem = () => screen.queryByRole('button', { name: 'New inbox item' })

describe('space header: New inbox item', () => {
  it('is offered on the overview to an operator of the space, and opens the inbox dialog', async () => {
    await show(me([{ role: 'operator', domain: 'spc_1' }]), '/spaces/spc_1')
    expect(screen.getByRole('button', { name: 'Launch run' })).toBeInTheDocument()
    fireEvent.click(newItem()!)
    expect(await screen.findByRole('dialog', { name: 'New inbox item' })).toBeInTheDocument()
    expect(await screen.findByRole('textbox', { name: 'Item text' })).toBeInTheDocument()
  })

  it('is offered through global grants and to the root', async () => {
    await show(me([{ role: 'admin', domain: '*' }]), '/spaces/spc_1')
    expect(newItem()).toBeInTheDocument()
  })

  it('is hidden from a viewer and from users without a grant in this space', async () => {
    await show(me([{ role: 'viewer', domain: 'spc_1' }, { role: 'admin', domain: 'spc_other' }]), '/spaces/spc_1')
    expect(newItem()).not.toBeInTheDocument()
  })

  it('is only on the overview: other space pages have their own actions', async () => {
    await show(me([{ role: 'operator', domain: 'spc_1' }]), '/spaces/spc_1/inbox')
    expect(screen.getByRole('button', { name: 'Launch run' })).toBeInTheDocument()
    expect(newItem()).not.toBeInTheDocument()
  })
})
