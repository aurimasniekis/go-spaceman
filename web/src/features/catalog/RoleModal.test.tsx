import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type SpaceRole } from '@/api/client'
import { keys } from '@/api/queries'
import { RoleModal } from './SpaceRolesCard'

const options = {
  templates: [{ id: 'rt_impl', name: 'implementor', kind: 'worker' }],
  profiles: [], prompts: [], skills: [],
}

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  Element.prototype.scrollIntoView ??= () => {}
})
afterEach(() => vi.restoreAllMocks())

function open(role?: SpaceRole) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.roleOptions('spc_1'), options)
  qc.setQueryData(keys.roleTemplates, [{ id: 'rt_impl', name: 'implementor', kind: 'worker', description: 'Makes the change the task describes' }])
  return render(<MantineProvider><QueryClientProvider client={qc}><RoleModal spaceId="spc_1" role={role} onClose={() => {}} /></QueryClientProvider></MantineProvider>)
}

describe('Space role description', () => {
  it('shows the template description as the placeholder and saves what was typed', async () => {
    const put = vi.spyOn(api, 'PUT').mockResolvedValue({ data: {}, response: new Response() } as never)
    open({ name: 'impl-web', template_id: 'rt_impl', revision: 1 } as SpaceRole)
    const field = screen.getByLabelText(/^Description/)
    expect(field).toHaveValue('')
    expect(field).toHaveAttribute('placeholder', 'Makes the change the task describes')
    fireEvent.change(field, { target: { value: 'Builds the web UI' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(put).toHaveBeenCalled())
    expect(put.mock.calls[0][1]).toMatchObject({ body: { description: 'Builds the web UI', template_id: 'rt_impl' } })
  })

  it('starts with the role’s own description', () => {
    open({ name: 'impl-web', template_id: 'rt_impl', description: 'Builds the web UI', revision: 1 } as SpaceRole)
    expect(screen.getByLabelText(/^Description/)).toHaveValue('Builds the web UI')
  })
})
