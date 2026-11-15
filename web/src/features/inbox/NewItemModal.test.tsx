import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { NewItemModal } from './NewItemModal'

// jsdom has no matchMedia or ResizeObserver; Mantine's modal transitions need them.
beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})

describe('new inbox item dialog', () => {
  it('focuses the body when it opens, not the title', async () => {
    render(
      <MantineProvider>
        <QueryClientProvider client={new QueryClient()}>
          <NewItemModal spaceId="spc_1" opened onClose={() => {}} onCreated={() => {}} />
        </QueryClientProvider>
      </MantineProvider>,
    )
    const body = await screen.findByRole('textbox', { name: 'Item text' })
    await waitFor(() => expect(body).toHaveFocus())
    expect(screen.getByLabelText(/Title/)).not.toHaveFocus()
  })
})
