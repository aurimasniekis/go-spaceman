import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me, type OperatorMessage } from '@/api/client'
import { keys } from '@/api/queries'
import { viewPrefs } from '@/lib/viewPrefs'
import { OperatorMessages } from './OperatorMessages'

const me = { method: 'password', user: { id: 'usr_1', display_name: 'Op', grants: [{ role: 'operator', domain: 'spc_1' }, { role: 'viewer', domain: 'spc_2' }] } } as unknown as Me
const msg = (id: string, over: Partial<OperatorMessage> = {}): OperatorMessage => ({ id, space_id: 'spc_1', space_name: 'Payments', space_slug: 'payments', kind: 'request',
  subject: `Subject ${id}`, body: '', state: 'available', from_kind: 'run', from_label: 'worker', to_kind: 'operators', created_at: new Date().toISOString(), version: 1, ...over })

beforeEach(() => {
  viewPrefs.setState(() => ({}))
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

const ok = (data: unknown) => Promise.resolve({ data, response: new Response() }) as never
/** GET /messages answers with the list; the spaces query answers with the two spaces. */
const list = (items: OperatorMessage[], unread = items.length) => (path: string) =>
  path === '/messages' ? ok({ items, unread_count: unread }) : ok([{ id: 'spc_1', name: 'Payments' }, { id: 'spc_2', name: 'Docs' }])

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(keys.me, me)
  const root = createRootRoute({ component: () => <OperatorMessages /> })
  const router = createRouter({ routeTree: root.addChildren([]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  return qc
}

describe('Needs you → Messages', () => {
  it('lists messages from every space with the space name linked, kind, sender and state', async () => {
    vi.spyOn(api, 'GET').mockImplementation(list([msg('a'), msg('b', { space_id: 'spc_2', space_name: 'Docs', kind: 'notification' })], 2) as never)
    mount()
    const row = await screen.findByRole('listitem', { name: 'Subject a, available' }, { timeout: 4000 })
    expect(row).toHaveTextContent('request')
    expect(row).toHaveTextContent('worker')
    expect(screen.getByRole('link', { name: 'Payments' })).toHaveAttribute('href', '/spaces/spc_1/messages')
    expect(screen.getByRole('link', { name: 'Docs' })).toHaveAttribute('href', '/spaces/spc_2/messages')
  })

  it('asks the server for unread by default and re-queries with the chosen filters', async () => {
    const get = vi.spyOn(api, 'GET').mockImplementation(list([msg('a')], 1) as never)
    mount()
    await screen.findByRole('listitem', { name: /Subject a/ }, { timeout: 4000 })
    const query = () => (get.mock.calls.filter((c) => c[0] === '/messages').at(-1)?.[1] as unknown as { params: { query: Record<string, unknown> } }).params.query
    expect(query()).toMatchObject({ state: 'open' })
    fireEvent.click(screen.getByLabelText('All'))
    await waitFor(() => expect(query()).toMatchObject({ state: 'all' }))
    fireEvent.click(screen.getByRole('textbox', { name: 'Kind' }))
    fireEvent.click(await screen.findByRole('option', { name: 'reply', hidden: true }))
    await waitFor(() => expect(query()).toMatchObject({ state: 'all', kind: 'reply' }))
  })

  it('acknowledges inline through the per-space operation and refreshes', async () => {
    const get = vi.spyOn(api, 'GET').mockImplementation(list([msg('a')], 1) as never)
    const post = vi.spyOn(api, 'POST').mockImplementation(() => ok({}))
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Acknowledge Subject a' }, { timeout: 4000 }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/messages/{message_id}/ack', { params: { path: { space_id: 'spc_1', message_id: 'a' } } }))
    await waitFor(() => expect(get.mock.calls.length).toBeGreaterThan(1))
  })

  it('keeps the opened message in the drawer after Acknowledge drops it from the Unread list', async () => {
    let items = [msg('a')]
    vi.spyOn(api, 'GET').mockImplementation(((path: string) => (list(items, items.length) as (p: string) => unknown)(path)) as never)
    vi.spyOn(api, 'POST').mockImplementation(() => { items = []; return ok(msg('a', { state: 'acknowledged', version: 2, acknowledged_at: new Date().toISOString(), acknowledged_by: 'Op' })) })
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Subject a' }, { timeout: 4000 }))
    const drawer = await screen.findByRole('dialog')
    fireEvent.click(await within(drawer).findByRole('button', { name: 'Acknowledge' }))
    await waitFor(() => expect(screen.queryByRole('listitem', { name: /Subject a/ })).toBeNull())
    expect(screen.getByRole('dialog')).toBe(drawer)
    expect(drawer).toHaveTextContent('Subject a')
    // The drawer shows the server's answer, not the stale row: acknowledged, and no second Acknowledge.
    await waitFor(() => expect(drawer).toHaveTextContent('acknowledged'))
    expect(drawer).not.toHaveTextContent('available')
    expect(within(drawer).queryByRole('button', { name: 'Acknowledge' })).toBeNull()
  })

  it('a cached older row (version 1, All filter) does not overwrite the drawer\'s version 2', async () => {
    // The list keeps answering with the version 1 row, as when the refetch has not landed yet.
    vi.spyOn(api, 'GET').mockImplementation(list([msg('a')], 1) as never)
    vi.spyOn(api, 'POST').mockImplementation(() => ok(msg('a', { state: 'acknowledged', version: 2, acknowledged_at: new Date().toISOString(), acknowledged_by: 'Op' })))
    mount()
    fireEvent.click(await screen.findByLabelText('All'))
    fireEvent.click(await screen.findByRole('button', { name: 'Subject a' }, { timeout: 4000 }))
    const drawer = await screen.findByRole('dialog')
    fireEvent.click(await within(drawer).findByRole('button', { name: 'Acknowledge' }))
    await waitFor(() => expect(drawer).toHaveTextContent('acknowledged'))
    // Let the invalidation refetch finish; the version 1 row it brings must not win.
    await new Promise((r) => setTimeout(r, 100))
    expect(drawer).toHaveTextContent('acknowledged')
    expect(within(drawer).queryByRole('button', { name: 'Acknowledge' })).toBeNull()
  })

  it('shows the request as resolved in the drawer after a reply resolves it', async () => {
    let items = [msg('a')]
    vi.spyOn(api, 'GET').mockImplementation(((path: string) =>
      path === '/spaces/{space_id}/messages/{message_id}' ? ok(msg('a', { state: 'resolved', version: 2, resolution: 'replied: r1' }))
        : (list(items, items.length) as (p: string) => unknown)(path)) as never)
    const post = vi.spyOn(api, 'POST').mockImplementation(() => { items = []; return ok(msg('r1', { kind: 'reply', from_kind: 'operator', to_kind: 'run' })) })
    mount()
    fireEvent.click(await screen.findByRole('button', { name: 'Subject a' }, { timeout: 4000 }))
    const drawer = await screen.findByRole('dialog')
    expect(drawer).toHaveTextContent('available')
    const box = await within(drawer).findByRole('textbox', { name: 'Reply' })
    fireEvent.input(box, { target: { textContent: 'on it' } })
    box.textContent = 'on it'
    fireEvent.input(box)
    fireEvent.click(await within(drawer).findByRole('button', { name: 'Send reply' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/messages/{message_id}/reply', expect.anything()))
    await waitFor(() => expect(drawer).toHaveTextContent('resolved'), { timeout: 4000 })
    expect(within(drawer).queryByRole('button', { name: 'Resolve' })).toBeNull()
  })

  it('offers no Acknowledge to a viewer, nor on an acknowledged message', async () => {
    vi.spyOn(api, 'GET').mockImplementation(list([msg('v', { space_id: 'spc_2' }), msg('d', { state: 'acknowledged' })], 0) as never)
    mount()
    await screen.findByRole('listitem', { name: /Subject v/ }, { timeout: 4000 })
    expect(screen.queryByRole('button', { name: /^Acknowledge/ })).toBeNull()
  })
})
