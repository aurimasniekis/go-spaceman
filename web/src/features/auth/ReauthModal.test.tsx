import { MantineProvider, Modal } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError, api } from '@/api/client'
import { keys } from '@/api/queries'
import { requestReauth } from '@/api/session'
import { ReauthModal } from './ReauthModal'

// jsdom has no matchMedia or ResizeObserver; Mantine's modal transitions need them.
beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => vi.restoreAllMocks())

function me(mfa: boolean) {
  return { method: 'password', csrf_token: 'c', limited: false, user: { id: 'usr_1', email: 'ada@example.com', mfa_enrolled: mfa } }
}

function mount(mfa: boolean, withDialog = false) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.me, me(mfa))
  return render(
    <MantineProvider>
      <QueryClientProvider client={qc}>
        <ReauthModal />
        {withDialog && <Modal opened onClose={() => {}} title="Add node">form</Modal>}
      </QueryClientProvider>
    </MantineProvider>,
  )
}

const ok = { data: me(true), error: undefined, response: new Response(null, { status: 200 }) }

describe('step-up prompt', () => {
  it('asks an account with an authenticator for its code, even when only a recent sign-in was asked for', async () => {
    const post = vi.spyOn(api, 'POST').mockResolvedValue(ok as never)
    mount(true)
    let done: Promise<boolean> = Promise.resolve(false)
    act(() => { done = requestReauth('reauth_required') })
    expect(await screen.findByText('This action needs your password and an authenticator code.')).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('Password'), { target: { value: 'pw' } })
    fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '123456' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await done).toBe(true)
    expect(post).toHaveBeenCalledWith('/auth/reauth', { body: { password: 'pw', code: '123456' } })
  })

  it('asks for the code when the server says the account needs one', async () => {
    const post = vi.spyOn(api, 'POST').mockRejectedValueOnce(new ApiError('multi-factor authentication required', 401, 'mfa_required'))
    mount(false)
    let done: Promise<boolean> = Promise.resolve(false)
    act(() => { done = requestReauth('reauth_required') })
    fireEvent.change(await screen.findByLabelText('Password'), { target: { value: 'pw' } })
    expect(screen.queryByLabelText('Authenticator code')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await screen.findByLabelText('Authenticator code')).toBeInTheDocument()
    expect(screen.getByText('Enter the code from your authenticator app as well.')).toBeInTheDocument()
    expect(screen.queryByText('multi-factor authentication required')).not.toBeInTheDocument()
    post.mockResolvedValueOnce(ok as never)
    fireEvent.change(screen.getByLabelText('Authenticator code'), { target: { value: '654321' } })
    fireEvent.click(screen.getByRole('button', { name: 'Continue' }))
    expect(await done).toBe(true)
    expect(post).toHaveBeenLastCalledWith('/auth/reauth', { body: { password: 'pw', code: '654321' } })
  })

  it('opens above the dialog that triggered it', async () => {
    mount(true, true)
    act(() => { void requestReauth('reauth_required') })
    const title = await screen.findByText("Confirm it's you")
    const z = (el: Element | null) => Number(getComputedStyle(el as Element).getPropertyValue('--mb-z-index') || 0)
    await waitFor(() => expect(z(title.closest('.mantine-Modal-root'))).toBeGreaterThan(z(screen.getByText('Add node').closest('.mantine-Modal-root'))))
  })
})
