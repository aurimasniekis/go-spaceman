import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me } from '@/api/client'
import { keys } from '@/api/queries'
import { Route } from './human'

const me = { method: 'password', user: { id: 'usr_1', display_name: 'Op', grants: [{ role: 'operator', domain: 'spc_1' }] } } as unknown as Me

beforeEach(() => {
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation((path: string) => Promise.resolve(path === '/messages'
    ? { data: { items: [], unread_count: 3 }, response: new Response() }
    : { data: { items: [], total: 0, counts: {} }, response: new Response() }) as never)
})
afterEach(() => vi.restoreAllMocks())

async function open(url: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(keys.me, me)
  const root = createRootRoute({ component: () => <Outlet /> })
  const human = Route.update({ id: '/human', path: '/human', getParentRoute: () => root } as never)
  const router = createRouter({ routeTree: root.addChildren([human]), history: createMemoryHistory({ initialEntries: [url] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  await screen.findByRole('tab', { name: /Messages/ }, { timeout: 4000 })
}

describe('Needs you tabs', () => {
  it('opens the actions list by default, with the unread count on the Messages tab', async () => {
    await open('/human')
    expect(screen.getByRole('tab', { name: 'Actions' })).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByRole('tab', { name: /Messages/ })).toHaveTextContent(/3/)
  }, 15000)
  it('opens the Messages tab for ?tab=messages', async () => {
    await open('/human?tab=messages')
    expect(screen.getByRole('tab', { name: /Messages/ })).toHaveAttribute('aria-selected', 'true')
    expect(await screen.findByRole('list', { name: 'Messages to operators' })).toBeInTheDocument()
  }, 15000)
})
