import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type McpServer } from '@/api/client'
import { keys } from '@/api/queries'
import { SpacemanMcpDetail } from './SpacemanMcpDetail'

const server = {
  id: 'mcp_1', slug: 'spaceman-admin', name: 'SpaceMan', kind: 'spaceman', enabled: true, version: 3, description: '', url: '', header_names: [], headers_version: 0,
  allow_private: false, tools: [], call_timeout_ms: 20000, max_concurrency: 4, max_result_bytes: 262144, grants: 1, created_at: '', updated_at: '',
  spaceman: { preset: 'admin', tools: [], approval: 'none', token_id: 'atk_internal1', owner_user_id: 'usr_1', grants: [{ role: 'admin', domain: '*' }], never_expires: true, token_state: 'active' },
} as unknown as McpServer

const tools = {
  items: [
    { name: 'get_space', tags: ['Spaces'], class: 'read', summary: 'A space', in_preset: true, selected: true, reachable: true },
    { name: 'delete_space', tags: ['Spaces'], class: 'dangerous', summary: 'Delete a space', in_preset: true, selected: true, reachable: true },
    { name: 'get_recording', tags: ['Runs'], class: 'sensitive-read', summary: 'A recording', in_preset: false, selected: false, reachable: true },
  ],
  count: 2,
}

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  vi.spyOn(api, 'GET').mockImplementation(((path: string) =>
    path === '/mcp-servers/spaceman/tools' ? Promise.resolve({ data: tools, response: new Response() }) : new Promise(() => {})) as never)
})

afterEach(() => vi.restoreAllMocks())

async function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.users, [{ id: 'usr_1', display_name: 'Ada Admin', email: 'ada@example.com' }])
  qc.setQueryData(keys.spaces, [])
  const router = createRouter({ routeTree: createRootRoute({ component: () => <>{ui}</> }), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(<MantineProvider><QueryClientProvider client={qc}><RouterProvider router={router} /></QueryClientProvider></MantineProvider>)
  await screen.findByText('Effective tools')
}

describe('SpaceMan MCP drawer', () => {
  it('shows the owner, the internal token, and the effective tools with dangerous ones flagged', async () => {
    await wrap(<SpacemanMcpDetail server={server} canWrite onDeleted={() => {}} />)
    expect(screen.getByText('Ada Admin (ada@example.com)')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'atk_internal1' })).toHaveAttribute('href', '/tokens')
    expect(screen.getByText(/can’t be copied/)).toBeInTheDocument()
    await waitFor(() => expect(screen.getByTestId('effective-count')).toHaveTextContent('2 tools'))
    expect(screen.getByText('get_space')).toBeInTheDocument()
    expect(screen.getByText('delete_space · approval')).toBeInTheDocument()
    expect(screen.queryByText('get_recording')).not.toBeInTheDocument()
  })

  it('confirms Re-issue, Disable and Delete before calling the API', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue({ data: server, response: new Response() } as never)
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: server, response: new Response() } as never)
    const del = vi.spyOn(api, 'DELETE').mockResolvedValue({ response: new Response(null, { status: 204 }) } as never)
    const onDeleted = vi.fn()
    await wrap(<SpacemanMcpDetail server={server} canWrite onDeleted={onDeleted} />)

    fireEvent.click(screen.getByRole('button', { name: 'Re-issue…' }))
    expect(post).not.toHaveBeenCalled()
    fireEvent.click(within(await screen.findByRole('dialog', { name: 'Re-issue the token?' })).getByRole('button', { name: 'Re-issue' }))
    await waitFor(() => expect(post).toHaveBeenCalledWith('/mcp-servers/{server_id}/reissue', {
      params: { path: { server_id: 'mcp_1' }, header: { 'If-Match': '"3"' } }, body: { grants: [{ role: 'admin', domain: '*' }], expires_in_days: undefined },
    }))

    fireEvent.click(screen.getByRole('button', { name: 'Disable…' }))
    expect(put).not.toHaveBeenCalled()
    fireEvent.click(within(await screen.findByRole('dialog', { name: /Disable this/ })).getByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(put).toHaveBeenCalledWith('/mcp-servers/{server_id}/spaceman', expect.objectContaining({ body: expect.objectContaining({ enabled: false, preset: 'admin', name: 'SpaceMan' }) })))

    fireEvent.click(screen.getByRole('button', { name: 'Delete…' }))
    const dialog = await screen.findByRole('dialog', { name: /Delete this/ })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(del).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: 'Delete…' }))
    fireEvent.click(within(await screen.findByRole('dialog', { name: /Delete this/ })).getByRole('button', { name: 'Delete' }))
    await waitFor(() => expect(onDeleted).toHaveBeenCalled())
    expect(del).toHaveBeenCalledWith('/mcp-servers/{server_id}', { params: { path: { server_id: 'mcp_1' } } })
  })

  it('offers no actions without mcp:write', async () => {
    await wrap(<SpacemanMcpDetail server={server} canWrite={false} onDeleted={() => {}} />)
    expect(screen.queryByRole('button', { name: /Re-issue|Disable|Delete/ })).not.toBeInTheDocument()
  })
})
