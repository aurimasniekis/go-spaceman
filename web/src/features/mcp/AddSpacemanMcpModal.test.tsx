import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Me } from '@/api/client'
import { keys } from '@/api/queries'
import { setReauthHandler } from '@/api/session'
import { AddSpacemanMcpModal } from './AddSpacemanMcpModal'
import type { SpacemanTool } from './spacemanMcp'

// openapi-fetch keeps the fetch and Request it found when the client was created: route fetch through a stub that
// tests swap, and let Request resolve the client's relative URLs (/api/v1/…) against the page like a browser does.
const net = vi.hoisted(() => {
  const BaseRequest = globalThis.Request
  globalThis.Request = class extends BaseRequest {
    constructor(input: RequestInfo | URL, init?: RequestInit) {
      super(typeof input === 'string' && input.startsWith('/') ? new URL(input, window.location.origin).toString() : input, init)
    }
  } as typeof Request
  const real = globalThis.fetch
  const state = { fetch: real as typeof fetch }
  globalThis.fetch = ((...a: Parameters<typeof fetch>) => state.fetch(...a)) as typeof fetch
  return state
})

const SPACE = 'spc_infra'

const me = (grants: { role: string; domain: string }[]) =>
  ({ root: false, method: 'password', limited: false, needs_enroll: false, domains: [], expires_at: '', session_id: 's', user: { id: 'usr_1', display_name: 'Ada', grants } }) as unknown as Me

const tool = (name: string, tag: string, cls: SpacemanTool['class'], reachable = true): SpacemanTool =>
  ({ name, tags: [tag], class: cls, summary: `${name} summary`, in_preset: false, selected: false, reachable })

const catalog = [tool('get_space', 'Spaces', 'read'), tool('delete_space', 'Spaces', 'dangerous'), tool('create_task', 'Tasks', 'write'), tool('list_nodes', 'Nodes', 'read', false)]
const classes: Record<string, string[]> = { read: ['read'], operate: ['read', 'write'], admin: ['read', 'write', 'dangerous'], custom: [] }

/** A stand-in for list-spaceman-mcp-tools: the preset's classes plus named tools, counted when reachable. */
function toolsAnswer(query: { preset?: string; tools?: string[] }) {
  const items = catalog.map((t) => {
    const inPreset = classes[query.preset ?? 'admin'].includes(t.class)
    return { ...t, in_preset: inPreset, selected: inPreset || (query.tools ?? []).includes(t.name) }
  })
  return { items, count: items.filter((t) => t.selected && t.reachable).length }
}

let get: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  Element.prototype.scrollIntoView ??= () => {}
  get = vi.spyOn(api, 'GET').mockImplementation(((path: string, init?: { params?: { query?: { preset?: string; tools?: string[] } } }) => {
    if (path === '/mcp-servers/spaceman/tools') return Promise.resolve({ data: toolsAnswer(init?.params?.query ?? {}), response: new Response() })
    return new Promise(() => {})
  }) as never)
})

afterEach(() => {
  vi.restoreAllMocks()
  net.fetch = globalThis.fetch
})

function wrap(ui: ReactNode, who: Me) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, who)
  qc.setQueryData(keys.spaces, [{ id: SPACE, name: 'Infra' }, { id: 'spc_web', name: 'Web' }])
  return render(<MantineProvider><QueryClientProvider client={qc}>{ui}</QueryClientProvider></MantineProvider>)
}

const radio = (label: RegExp) => screen.getByRole('radio', { name: label })

describe('Add SpaceMan MCP', () => {
  it('defaults to full administration, the control role, dangerous approvals on and no expiry', async () => {
    wrap(<AddSpacemanMcpModal opened onClose={() => {}} spaceId={SPACE} />, me([{ role: 'admin', domain: '*' }]))
    expect(screen.getByLabelText(/^Slug/)).toHaveValue('spaceman-admin')
    expect(screen.getByLabelText(/^Name/)).toHaveValue('SpaceMan')
    expect(screen.getByLabelText(/^Space role/)).toHaveValue('control')
    expect(radio(/Administer everything \(infra\)/)).toBeChecked()
    for (const r of screen.getAllByRole('radio')) expect(r).toBeEnabled()
    expect(screen.getByRole('checkbox', { name: /Dangerous tools: always/ })).toBeChecked()
    expect(screen.getByRole('checkbox', { name: /Dangerous tools: always/ })).toBeDisabled()
    expect(screen.getByRole('switch', { name: /Writes need approval/ })).not.toBeChecked()
    expect(screen.getByRole('checkbox', { name: /Never expires/ })).toBeChecked()
    // admin@* with the admin preset: every reachable read, write and dangerous tool.
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('3 tools'))
    expect(get).toHaveBeenCalledWith('/mcp-servers/spaceman/tools', expect.objectContaining({ params: { query: { preset: 'admin', tools: undefined, grants: ['admin@*'] } } }))
  })

  it('disables the scopes beyond the caller’s grants and follows the scope’s preset', async () => {
    wrap(<AddSpacemanMcpModal opened onClose={() => {}} spaceId={SPACE} />, me([{ role: 'operator', domain: SPACE }]))
    expect(radio(/Administer everything/)).toBeDisabled()
    expect(radio(/Operate all spaces/)).toBeDisabled()
    expect(radio(/Read-only, all spaces/)).toBeDisabled()
    expect(radio(/Operate this space/)).toBeEnabled()
    expect(radio(/Operate this space/)).toBeChecked()
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('2 tools'))
    fireEvent.click(radio(/Read-only, this space/))
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('1 tool'))
    expect(get).toHaveBeenLastCalledWith('/mcp-servers/spaceman/tools', expect.objectContaining({ params: { query: { preset: 'read', tools: undefined, grants: [`viewer@${SPACE}`] } } }))
  })

  it('picks custom tools by tag, flags dangerous ones and keeps unreachable ones out', async () => {
    wrap(<AddSpacemanMcpModal opened onClose={() => {}} spaceId={SPACE} />, me([{ role: 'admin', domain: '*' }]))
    fireEvent.click(screen.getByRole('radio', { name: 'Custom' }))
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('0 tools'))
    expect(screen.getByRole('button', { name: 'Add server' })).toBeDisabled()
    fireEvent.click(screen.getByRole('button', { name: /Spaces/ }))
    expect(screen.getByText('dangerous · approval')).toBeInTheDocument()
    fireEvent.click(screen.getByRole('checkbox', { name: 'delete_space', hidden: true }))
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('1 tool'))
    expect(get).toHaveBeenLastCalledWith('/mcp-servers/spaceman/tools', expect.objectContaining({ params: { query: { preset: 'custom', tools: ['delete_space'], grants: ['admin@*'] } } }))
    fireEvent.click(screen.getByRole('button', { name: /Nodes/ }))
    expect(screen.getByRole('checkbox', { name: 'list_nodes', hidden: true })).toBeDisabled()
    expect(screen.getByText('beyond grants')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Add server' })).toBeEnabled()
  })

  it('refuses the reserved slug', () => {
    wrap(<AddSpacemanMcpModal opened onClose={() => {}} />, me([{ role: 'admin', domain: '*' }]))
    fireEvent.change(screen.getByLabelText(/^Slug/), { target: { value: 'spaceman' } })
    expect(screen.getByText(/reserved for the built-in tools/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Add server' })).toBeDisabled()
  })

  it('asks for a step-up, retries once, then grants the server to the space role', async () => {
    // The real client and session middleware: the first create answers reauth_required, the prompt succeeds, and the
    // same request is sent again.
    const sent: { method: string; path: string; body: unknown }[] = []
    let creates = 0
    net.fetch = (async (input: RequestInfo | URL) => {
      const req = input as Request
      const path = new URL(req.url).pathname
      sent.push({ method: req.method, path, body: await req.clone().json().catch(() => null) })
      if (path === '/api/v1/mcp-servers/spaceman' && creates++ === 0) {
        return new Response(JSON.stringify({ code: 'reauth_required', status: 401, title: 'Unauthorized' }), { status: 401, headers: { 'Content-Type': 'application/problem+json' } })
      }
      if (path === '/api/v1/mcp-servers/spaceman') {
        return new Response(JSON.stringify({ id: 'mcp_1', name: 'SpaceMan', slug: 'spaceman-admin', kind: 'spaceman', version: 1 }), { status: 201, headers: { 'Content-Type': 'application/json' } })
      }
      return new Response(JSON.stringify({ id: 'mgr_1' }), { status: 201, headers: { 'Content-Type': 'application/json' } })
    }) as typeof fetch
    const prompt = vi.fn(async () => true)
    const unregister = setReauthHandler(prompt)
    const onCreated = vi.fn()
    const onClose = vi.fn()
    try {
      wrap(<AddSpacemanMcpModal opened onClose={onClose} spaceId={SPACE} onCreated={onCreated} />, me([{ role: 'admin', domain: '*' }]))
      await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('3 tools'))
      fireEvent.click(screen.getByRole('button', { name: 'Add server' }))
      await waitFor(() => expect(onCreated).toHaveBeenCalled())
    } finally {
      unregister()
    }
    expect(prompt).toHaveBeenCalledWith('reauth_required')
    const creates2 = sent.filter((s) => s.path === '/api/v1/mcp-servers/spaceman')
    expect(creates2).toHaveLength(2)
    expect(creates2[1].body).toEqual({ slug: 'spaceman-admin', name: 'SpaceMan', preset: 'admin', tools: [], approval: 'none', grants: [{ role: 'admin', domain: '*' }] })
    expect(sent.find((s) => s.path === '/api/v1/mcp-servers/mcp_1/grants')?.body).toEqual({ space_id: SPACE, role: 'control', tools: [] })
    expect(onClose).toHaveBeenCalled()
  })

  it('stays open with a warning when granting the created server fails, and a retry only grants', async () => {
    const sent: string[] = []
    let grants = 0
    net.fetch = (async (input: RequestInfo | URL) => {
      const path = new URL((input as Request).url).pathname
      sent.push(path)
      if (path === '/api/v1/mcp-servers/spaceman') {
        return new Response(JSON.stringify({ id: 'mcp_1', name: 'SpaceMan', slug: 'spaceman-admin', kind: 'spaceman', version: 1 }), { status: 201, headers: { 'Content-Type': 'application/json' } })
      }
      if (grants++ === 0) {
        return new Response(JSON.stringify({ code: 'invalid', status: 422, title: 'Unprocessable Entity', detail: 'bad role' }), { status: 422, headers: { 'Content-Type': 'application/problem+json' } })
      }
      return new Response(JSON.stringify({ id: 'mgr_1' }), { status: 201, headers: { 'Content-Type': 'application/json' } })
    }) as typeof fetch
    const onCreated = vi.fn()
    const onClose = vi.fn()
    wrap(<AddSpacemanMcpModal opened onClose={onClose} spaceId={SPACE} onCreated={onCreated} />, me([{ role: 'admin', domain: '*' }]))
    await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('3 tools'))
    fireEvent.click(screen.getByRole('button', { name: 'Add server' }))
    expect(await screen.findByText(/SpaceMan was created, but it has no grant yet: granting it failed/)).toBeInTheDocument()
    expect(onClose).not.toHaveBeenCalled()
    expect(onCreated).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Retry grant' }))
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(expect.objectContaining({ id: 'mcp_1' })))
    expect(onClose).toHaveBeenCalled()
    expect(sent.filter((p) => p === '/api/v1/mcp-servers/spaceman')).toHaveLength(1)
    expect(sent.filter((p) => p === '/api/v1/mcp-servers/mcp_1/grants')).toHaveLength(2)
  })

  it('does not create the server when the step-up is cancelled', async () => {
    const fetchSpy = vi.fn(async () =>
      new Response(JSON.stringify({ code: 'reauth_required', status: 401, title: 'Unauthorized', detail: 'a recent sign-in is required' }), { status: 401, headers: { 'Content-Type': 'application/problem+json' } }))
    net.fetch = fetchSpy as unknown as typeof fetch
    const unregister = setReauthHandler(async () => false)
    const onCreated = vi.fn()
    try {
      wrap(<AddSpacemanMcpModal opened onClose={() => {}} spaceId={SPACE} onCreated={onCreated} />, me([{ role: 'admin', domain: '*' }]))
      await waitFor(() => expect(screen.getByTestId('tool-count')).toHaveTextContent('3 tools'))
      fireEvent.click(screen.getByRole('button', { name: 'Add server' }))
      await waitFor(() => expect(within(screen.getByRole('dialog')).getByRole('alert')).toBeInTheDocument())
    } finally {
      unregister()
    }
    expect(fetchSpy).toHaveBeenCalledTimes(1)
    expect(onCreated).not.toHaveBeenCalled()
  })
})
