import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { theme } from '@/theme/theme'
import { SpaceUsageCard } from './UsageCards'

const SPACE = 'spc_1'
const usage = { known_runs: 2, unknown_runs: 0, input_tokens: 1000, output_tokens: 200, cache_read_tokens: 0, cache_write_tokens: 0, estimated_runs: 0, estimated_usd: 0 }

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})
afterEach(() => vi.restoreAllMocks())

describe('SpaceUsageCard', () => {
  it('links to the space\'s usage over time', async () => {
    vi.spyOn(api, 'GET').mockImplementation(((path: string) =>
      path === '/spaces/{space_id}/usage' ? Promise.resolve({ data: usage, response: new Response() }) : new Promise(() => {})) as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const router = createRouter({
      routeTree: createRootRoute({
        component: () => (
          <MantineProvider theme={theme}>
            <QueryClientProvider client={qc}>
              <SpaceUsageCard spaceId={SPACE} />
            </QueryClientProvider>
          </MantineProvider>
        ),
      }),
      history: createMemoryHistory(),
    })
    await router.load()
    render(<RouterProvider router={router} />)
    const link = await screen.findByRole('link', { name: 'Usage over time' })
    expect(link.getAttribute('href')).toBe(`/spaces/${SPACE}/usage`)
  })
})
