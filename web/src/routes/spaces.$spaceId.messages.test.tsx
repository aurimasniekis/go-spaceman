import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me } from '@/api/client'
import { keys } from '@/api/queries'
import { Route } from './spaces.$spaceId.messages'

const me = { method: 'password', user: { id: 'usr_1', display_name: 'Op', grants: [{ role: 'operator', domain: 'spc_1' }] } } as unknown as Me

beforeEach(() => {
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})
afterEach(() => vi.restoreAllMocks())

/** The messages route as the generated route tree attaches it, opened at the given URL. */
async function open(url: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, me)
  qc.setQueryData(keys.messages('spc_1'), { items: [], operators_pending: 0 })
  qc.setQueryData(keys.scheduledMessages('spc_1'), [])
  const root = createRootRoute({ component: () => <Outlet /> })
  const messages = Route.update({ id: '/spaces/$spaceId/messages', path: 'spaces/$spaceId/messages', getParentRoute: () => root } as never)
  const router = createRouter({ routeTree: root.addChildren([messages]), history: createMemoryHistory({ initialEntries: [url] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  await screen.findByRole('radiogroup', { name: 'Which messages' })
}

const selected = () => (screen.getByRole('radiogroup', { name: 'Which messages' }).querySelector('input:checked') as HTMLInputElement).value

describe('messages route: ?view', () => {
  it('opens the System view for ?view=system, where a run dialog notice is', async () => {
    await open('/spaces/spc_1/messages?view=system')
    expect(selected()).toBe('system')
  })
  it.each([
    ['no view', '/spaces/spc_1/messages'],
    ['an unknown view', '/spaces/spc_1/messages?view=scheduled'],
    ['an invalid view', '/spaces/spc_1/messages?view=%7B%7D'],
  ])('opens People and agents for %s', async (_n, url) => {
    await open(url)
    expect(selected()).toBe('people')
  })
})
