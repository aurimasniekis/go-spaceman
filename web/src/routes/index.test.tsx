import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me } from '@/api/client'
import { keys } from '@/api/queries'
import { Route } from './index'

const me = { method: 'password', user: { id: 'usr_1', display_name: 'Op', grants: [{ role: 'operator', domain: 'spc_1' }] } } as unknown as Me
const never = () => new Promise<never>(() => {})
const ok = (data: unknown) => Promise.resolve({ data, response: { status: 200, ok: true } })
const fail = () => Promise.resolve({ error: { code: 'internal', message: 'boom' }, response: { status: 500, ok: false } })

beforeEach(() => {
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

/** The Overview route as the route tree attaches it; every read not named in `reads` stays pending. */
async function open(reads: Record<string, () => Promise<unknown>>) {
  vi.spyOn(api, 'GET').mockImplementation(((path: string) => (reads[path] ?? never)()) as never)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(keys.me, me)
  qc.setQueryData(keys.spaces, [])
  const root = createRootRoute({ component: () => <Outlet /> })
  const index = Route.update({ id: '/', path: '/', getParentRoute: () => root } as never)
  const router = createRouter({ routeTree: root.addChildren([index]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  return { attention: await screen.findByTestId('attention-top', {}, { timeout: 4000 }) }
}

describe('Overview route: attention and usage states', () => {
  it('shows Loading while the attention and usage reads are pending, never "nothing needs you" or figures', async () => {
    const { attention } = await open({})
    expect(within(attention).getByRole('status').textContent).toBe('Loading…')
    expect(screen.queryByText('Nothing needs you right now.')).toBeNull()
    expect(screen.getByText('Usage').closest('div')?.textContent ?? '').not.toMatch(/tok/)
    expect(screen.getAllByRole('status').every((s) => s.textContent === 'Loading…')).toBe(true)
  }, 15000)

  it('shows each read\'s error, not an empty state, when the reads fail', async () => {
    const { attention } = await open({ '/attention': fail, '/usage/series': fail })
    expect(await within(attention).findByText('Attention is unavailable')).toBeTruthy()
    expect(await screen.findByText('Usage is unavailable')).toBeTruthy()
    expect(screen.queryByText('Nothing needs you right now.')).toBeNull()
  }, 15000)

  it('shows the empty state once attention has loaded with no items', async () => {
    await open({ '/attention': () => ok({ items: [], total: 0 }) })
    expect(await screen.findByText('Nothing needs you right now.')).toBeTruthy()
  }, 15000)
})
