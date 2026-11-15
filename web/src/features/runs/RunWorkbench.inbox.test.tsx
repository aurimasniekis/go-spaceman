import { MantineProvider } from '@mantine/core'
import { Notifications } from '@mantine/notifications'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type InboxItem, type Me, type Run } from '@/api/client'
import { keys } from '@/api/queries'
import { NARROW_QUERY } from '@/lib/media'
import { RunWorkbench } from './RunWorkbench'

// ghostty-web needs WebAssembly and a canvas; these tests are about the actions around the terminal.
vi.mock('@/features/terminal/TerminalView', () => ({ TerminalView: () => <div data-testid="terminal" /> }))

const SPACE = 'spc_1'
let narrow = false

const run = (p: Partial<Run> = {}): Run => ({
  id: 'run_1', role: 'implementor', kind: 'worker', harness: 'claude-code', observed: 'running', desired: 'running', status: 'working', turn: 3, space_id: SPACE,
  created_at: '2026-11-13T08:00:00Z', started_at: '2026-11-13T08:00:00Z', created_by: 'user:usr_1', instance_id: 'ins_1', generation: 1, node_id: 'nod_1',
  launch: {}, ...p,
}) as unknown as Run

const me = (grants: { role: string; domain: string }[]): Me => ({ method: 'password', user: { id: 'usr_1', display_name: 'Op', grants } }) as unknown as Me
const operator = me([{ role: 'operator', domain: SPACE }])

let post: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  narrow = false
  window.matchMedia = ((q: string) => ({
    matches: narrow && q === NARROW_QUERY, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  window.scrollTo = () => {}
  // The editor scrolls a prefilled text's selection into view; jsdom has no layout.
  const rects = () => ({ length: 0, item: () => null, [Symbol.iterator]: [][Symbol.iterator] }) as unknown as DOMRectList
  Range.prototype.getClientRects ??= rects
  Range.prototype.getBoundingClientRect ??= () => new DOMRect()
  Element.prototype.getClientRects ??= rects
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
  post = vi.spyOn(api, 'POST').mockImplementation(() => new Promise(() => {}))
})
afterEach(() => vi.restoreAllMocks())

async function show(who: Me, r: Run = run()) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, who)
  qc.setQueryData(keys.nodes, [])
  const root = createRootRoute({ component: () => <Outlet /> })
  const page = createRoute({ getParentRoute: () => root, path: '/', component: () => <RunWorkbench spaceId={SPACE} run={r} mode="live" onMode={() => {}} inspectorDefault={false} /> })
  const runPage = createRoute({ getParentRoute: () => root, path: 'spaces/$spaceId/runs/$runId' })
  const item = createRoute({ getParentRoute: () => root, path: 'spaces/$spaceId/inbox/$itemId' })
  const router = createRouter({ routeTree: root.addChildren([page, runPage, item]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(
    <MantineProvider>
      <Notifications />
      <QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider>
    </MantineProvider>,
  )
  await screen.findByTestId('run-header')
}

const header = () => screen.getByTestId('run-header-actions')
const sendAsItem = () => screen.queryByRole('button', { name: 'Send as inbox item' })
const message = () => screen.getByRole('textbox', { name: 'Message the agent' }) as HTMLInputElement

describe('terminal view: inbox actions', () => {
  it('offers both to an operator of the space, who cannot take over the terminal', async () => {
    await show(operator)
    expect(within(header()).queryByRole('button', { name: /Take over/ })).not.toBeInTheDocument()
    expect(within(header()).getByRole('button', { name: 'New inbox item' })).toHaveTextContent('New inbox item')
    expect(sendAsItem()).toBeInTheDocument()
  })

  it('puts New inbox item next to Take over', async () => {
    await show(me([{ role: 'admin', domain: SPACE }]))
    const buttons = within(header()).getAllByRole('button').map((b) => b.getAttribute('aria-label') ?? b.textContent)
    const take = buttons.findIndex((b) => /Take over/.test(b ?? ''))
    expect(take).toBeGreaterThanOrEqual(0)
    expect(buttons[take + 1]).toBe('New inbox item')
  })

  it('hides both from a viewer and from an operator of another space', async () => {
    await show(me([{ role: 'viewer', domain: SPACE }, { role: 'admin', domain: 'spc_other' }]))
    expect(within(header()).queryByRole('button', { name: 'New inbox item' })).not.toBeInTheDocument()
    expect(sendAsItem()).not.toBeInTheDocument()
  })

  it('keeps New inbox item while someone else holds the terminal', async () => {
    await show(operator, run({ takeover: { id: 'tko_1', holder: 'user:usr_2', holder_label: 'Ada', started_at: '2026-11-13T09:00:00Z' } } as Partial<Run>))
    expect(within(header()).getByRole('button', { name: 'New inbox item' })).toBeInTheDocument()
  })

  it('uses icon buttons on a phone', async () => {
    narrow = true
    await show(operator)
    expect(within(header()).getByRole('button', { name: 'New inbox item' })).toHaveTextContent('')
    expect(sendAsItem()).toHaveTextContent('')
  })

  it('opens the header dialog empty', async () => {
    await show(operator)
    fireEvent.click(within(header()).getByRole('button', { name: 'New inbox item' }))
    const dialog = await screen.findByRole('dialog', { name: 'New inbox item' })
    const text = await within(dialog).findByRole('textbox', { name: 'Item text' })
    expect(text).toHaveTextContent('')
  })
})

describe('terminal composer: Send as inbox item', () => {
  it('is disabled until there is text', async () => {
    await show(operator)
    expect(sendAsItem()).toBeDisabled()
    fireEvent.change(message(), { target: { value: '   ' } })
    expect(sendAsItem()).toBeDisabled()
    fireEvent.change(message(), { target: { value: 'Look at the flaky test' } })
    expect(sendAsItem()).toBeEnabled()
  })

  it('opens the dialog prefilled with the text and the run, keeps the text when closed, and sends nothing to the terminal', async () => {
    await show(operator)
    fireEvent.change(message(), { target: { value: 'Look at the flaky test' } })
    fireEvent.click(sendAsItem()!)
    const dialog = await screen.findByRole('dialog', { name: 'New inbox item' })
    const text = await within(dialog).findByRole('textbox', { name: 'Item text' })
    await waitFor(() => expect(text).toHaveTextContent('Look at the flaky test'))
    expect(text).toHaveTextContent('From the terminal of implementor (run run_1)')
    expect(within(text).getByRole('link', { name: 'implementor' })).toHaveAttribute('href', expect.stringMatching(/\/spaces\/spc_1\/runs\/run_1$/))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New inbox item' })).not.toBeInTheDocument())
    expect(message().value).toBe('Look at the flaky test')
    expect(post).not.toHaveBeenCalled()
  })

  it('clears the message box once the item is added and confirms it with a link', async () => {
    post.mockResolvedValue({ data: { id: 'inb_1', title: 'Look at the flaky test' } as InboxItem, response: new Response() } as never)
    await show(operator)
    fireEvent.change(message(), { target: { value: 'Look at the flaky test' } })
    fireEvent.click(sendAsItem()!)
    const dialog = await screen.findByRole('dialog', { name: 'New inbox item' })
    const add = within(dialog).getByRole('button', { name: 'Add to inbox' })
    await waitFor(() => expect(add).toBeEnabled())
    fireEvent.click(add)

    const link = await screen.findByRole('link', { name: 'Look at the flaky test' })
    expect(link).toHaveAttribute('href', '/spaces/spc_1/inbox/inb_1')
    await waitFor(() => expect(message().value).toBe(''))
    const paths = post.mock.calls.map((c: unknown[]) => c[0])
    expect(paths).toEqual(['/spaces/{space_id}/inbox'])
    const [, init] = post.mock.calls[0] as unknown as [string, { body: { body: string } }]
    expect(init.body.body).toContain('Look at the flaky test')
    expect(init.body.body).toContain('run_1')
  })

  it('keeps the message box when adding fails', async () => {
    post.mockResolvedValue({ error: { title: 'Forbidden', status: 403, detail: 'no' }, response: new Response(null, { status: 403 }) } as never)
    await show(operator)
    fireEvent.change(message(), { target: { value: 'Look at the flaky test' } })
    fireEvent.click(sendAsItem()!)
    const dialog = await screen.findByRole('dialog', { name: 'New inbox item' })
    const add = within(dialog).getByRole('button', { name: 'Add to inbox' })
    await waitFor(() => expect(add).toBeEnabled())
    fireEvent.click(add)
    await waitFor(() => expect(post).toHaveBeenCalled())
    expect(await within(dialog).findByRole('alert')).toBeInTheDocument()
    expect(message().value).toBe('Look at the flaky test')
  })
})
