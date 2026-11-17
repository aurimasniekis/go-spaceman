import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, ApiError, type Prompt } from '@/api/client'
import { EditDetailsModal } from './EditDetailsModal'

const prompt = { id: 'prm_1', name: 'Reviewer', slug: 'reviewer', description: 'Reviews diffs', version: 3, kind: 'role', latest: 1 } as unknown as Prompt

let patch: ReturnType<typeof vi.spyOn>

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  patch = vi.spyOn(api, 'PATCH')
})
afterEach(() => vi.restoreAllMocks())

function open(onClose = () => {}) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(<MantineProvider><QueryClientProvider client={qc}><EditDetailsModal prompt={prompt} onClose={onClose} /></QueryClientProvider></MantineProvider>)
}

describe('Edit details', () => {
  it('sends the new name and description with the prompt version in If-Match', async () => {
    patch.mockResolvedValue({ data: { ...prompt }, response: new Response() } as never)
    const onClose = vi.fn()
    open(onClose)
    expect(screen.getByLabelText(/^Name/)).toHaveValue('Reviewer')
    fireEvent.change(screen.getByLabelText('Description'), { target: { value: 'Reviews pull requests' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(onClose).toHaveBeenCalled())
    expect(patch).toHaveBeenCalledWith('/prompts/{prompt_id}', {
      params: { path: { prompt_id: 'prm_1' }, header: { 'If-Match': '"3"' } },
      body: { name: 'Reviewer', description: 'Reviews pull requests' },
    })
  })

  it.each([412, 409])('shows the conflict message for a stale version (%i)', async (status) => {
    patch.mockResolvedValue({ error: { title: 'conflict' }, response: new Response(null, { status }) } as never)
    const onClose = vi.fn()
    open(onClose)
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/Someone else changed this prompt/)
    expect(onClose).not.toHaveBeenCalled()
  })

  it('shows another error as it is', async () => {
    patch.mockRejectedValue(new ApiError('name is required', 422, 'invalid', {}))
    open()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('name is required')
  })
})
