import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
    const row = await screen.findByRole('listitem', { name: 'Subject a, available' })
    expect(row).toHaveTextContent('request')
    expect(row).toHaveTextContent('worker')
    expect(screen.getByRole('link', { name: 'Payments' })).toHaveAttribute('href', '/spaces/spc_1/messages')
    expect(screen.getByRole('link', { name: 'Docs' })).toHaveAttribute('href', '/spaces/spc_2/messages')
  })

  it('asks the server for unread by default and re-queries with the chosen filters', async () => {
    const get = vi.spyOn(api, 'GET').mockImplementation(list([msg('a')], 1) as never)
    mount()
    await screen.findByRole('listitem', { name: /Subject a/ })
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
    fireEvent.click(await screen.findByRole('button', { name: 'Acknowledge Subject a' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/spaces/{space_id}/messages/{message_id}/ack', { params: { path: { space_id: 'spc_1', message_id: 'a' } } }))
    await waitFor(() => expect(get.mock.calls.length).toBeGreaterThan(1))
  })

  it('offers no Acknowledge to a viewer, nor on an acknowledged message', async () => {
    vi.spyOn(api, 'GET').mockImplementation(list([msg('v', { space_id: 'spc_2' }), msg('d', { state: 'acknowledged' })], 0) as never)
    mount()
    await screen.findByRole('listitem', { name: /Subject v/ })
    expect(screen.queryByRole('button', { name: /^Acknowledge/ })).toBeNull()
  })
})
