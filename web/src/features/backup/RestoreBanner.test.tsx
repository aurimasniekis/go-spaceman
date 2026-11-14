import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { RestoreBanner } from './RestoreBanner'

vi.mock('@tanstack/react-router', () => ({ Link: ({ children }: { children: ReactNode }) => <a href="/backups">{children}</a> }))

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

const state = (extra: Record<string, unknown> = {}) => ({
  backup_id: 'bkp_x', backup_created_at: '2026-11-13T17:25:57Z', restored_at: '2026-11-13T18:00:00Z', applied_at: '2026-11-13T18:00:01Z',
  paused_spaces: ['spc_a', 'spc_b', 'spc_c'], resumed_spaces: [], ...extra,
})

function show(body: unknown) {
  vi.spyOn(api, 'GET').mockResolvedValue({ data: body, response: new Response() } as never)
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(<QueryClientProvider client={qc}><MantineProvider><RestoreBanner canAcknowledge /></MantineProvider></QueryClientProvider>)
}

describe('RestoreBanner', () => {
  it('right after a restore names the spaces the restore paused and still holds', async () => {
    show({ restored: true, still_paused: 3, state: state() })
    expect(await screen.findByText(/3 of 3 space\(s\)/)).toBeInTheDocument()
  })

  it('counts only what the server says is still paused by the restore, not later operator pauses', async () => {
    // Restore paused three; all were resumed; the operator then paused one: the server reports 0 still paused.
    show({ restored: true, still_paused: 0, state: state({ resumed_spaces: ['spc_a', 'spc_b', 'spc_c'], acknowledged_at: '2026-11-13T19:00:00Z' }) })
    await waitFor(() => expect(api.GET).toHaveBeenCalled())
    await waitFor(() => expect(screen.queryByText(/Restored from a backup/)).not.toBeInTheDocument())
  })

  it('shows nothing when the installation was never restored', async () => {
    show({ restored: false, still_paused: 0 })
    await waitFor(() => expect(api.GET).toHaveBeenCalled())
    expect(screen.queryByText(/Restored from a backup/)).not.toBeInTheDocument()
  })
})
