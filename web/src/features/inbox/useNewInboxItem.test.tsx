import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type InboxItem } from '@/api/client'
import { useNewInboxItem } from './useNewInboxItem'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

afterEach(() => vi.restoreAllMocks())

function Opener() {
  const { open, modal } = useNewInboxItem('spc_1')
  return (
    <>
      <button type="button" onClick={() => open({ body: 'Look at **this**' })}>Send as item</button>
      {modal}
    </>
  )
}

async function show() {
  const root = createRootRoute({ component: () => <Outlet /> })
  const page = createRoute({ getParentRoute: () => root, path: '/', component: Opener })
  const item = createRoute({ getParentRoute: () => root, path: 'spaces/$spaceId/inbox/$itemId', component: () => <p>item page</p> })
  const router = createRouter({ routeTree: root.addChildren([page, item]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(
    <MantineProvider>
      <Notifications />
      <QueryClientProvider client={new QueryClient()}><RouterProvider router={router} /></QueryClientProvider>
    </MantineProvider>,
  )
  await screen.findByRole('button', { name: 'Send as item' })
  return router
}

describe('useNewInboxItem', () => {
  it('opens the dialog prefilled, adds the item and confirms it with a link to the item', async () => {
    const created = { id: 'inb_1', title: 'Look at this' } as InboxItem
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: created, response: new Response() } as never)
    const router = await show()
    fireEvent.click(screen.getByRole('button', { name: 'Send as item' }))
    await screen.findByRole('dialog', { name: 'New inbox item' })
    const add = screen.getByRole('button', { name: 'Add to inbox' })
    await waitFor(() => expect(add).toBeEnabled())
    fireEvent.click(add)

    const link = await screen.findByRole('link', { name: 'Look at this' })
    const [, init] = post.mock.calls[0] as unknown as [string, { body: { body: string; content_type: string } }]
    expect(init.body.content_type).toBe('text/markdown')
    expect(init.body.body).toContain('Look at **this**')
    expect(link).toHaveAttribute('href', '/spaces/spc_1/inbox/inb_1')
    fireEvent.click(link)
    await waitFor(() => expect(router.state.location.pathname).toBe('/spaces/spc_1/inbox/inb_1'))
  })
})
