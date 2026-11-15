import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from './client'
import { keys, useScheduledMessages } from './queries'

afterEach(() => vi.restoreAllMocks())

describe('useScheduledMessages', () => {
  it('asks the server for the pending scheduled messages (ADR-0065, review F3)', async () => {
    const items = [{ id: 'msg_1', state: 'stored', deliver_at: '2026-12-01T00:00:00Z' }]
    const get = vi.spyOn(api, 'GET').mockResolvedValue({ data: { items }, response: new Response() } as never)
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const wrapper = ({ children }: { children: ReactNode }) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>
    const { result } = renderHook(() => useScheduledMessages('spc_a'), { wrapper })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(get).toHaveBeenCalledWith('/spaces/{space_id}/messages', { params: { path: { space_id: 'spc_a' }, query: { scheduled: true } }, signal: expect.any(AbortSignal) })
    expect(result.current.data).toEqual(items)
    // Invalidating a space's messages (watch, send, cancel) refreshes the scheduled list too.
    await qc.invalidateQueries({ queryKey: keys.messages('spc_a') })
    expect(get).toHaveBeenCalledTimes(2)
  })
})
