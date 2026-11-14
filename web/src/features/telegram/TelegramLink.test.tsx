import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api } from '@/api/client'
import { ConfirmLink, POLL_MS, pollInterval } from './TelegramLink'

vi.mock('@/features/notifications/timezone', async (orig) => ({ ...(await orig<object>()), browserZone: () => 'Europe/Vilnius' }))

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
})
afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

const expires = new Date(Date.now() + 600_000).toISOString()
const code = (over: object = {}) => ({ id: 'cid', state: 'new', expires_at: expires, code: 'c', deep_link: 'https://t.me/bot?start=c', ...over })
const answer = (data: unknown) => ({ data, error: undefined, response: new Response(null, { status: 200 }) }) as never

function mount(initial: object, prefsZone = '') {
  const get = vi.spyOn(api, 'GET')
  const state = { current: initial as Record<string, unknown> }
  get.mockImplementation(((path: string) => Promise.resolve(answer(path.includes('notification') ? { timezone: prefsZone } : state.current))) as never)
  const onDone = vi.fn()
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const ui = render(
    <MantineProvider>
      <QueryClientProvider client={qc}>
        <ConfirmLink code={initial as never} onDone={onDone} />
      </QueryClientProvider>
    </MantineProvider>,
  )
  return { get, state, onDone, ui }
}

describe('confirm page poll', () => {
  it('polls every 2 s while the code can still change, and stops when it can not or has expired', () => {
    const now = Date.parse(expires) - 1000
    for (const s of ['new', 'redeemed', 'telegram_confirmed']) expect(pollInterval(s, expires, now)).toBe(POLL_MS)
    for (const s of ['confirmed', 'cancelled', 'expired', undefined]) expect(pollInterval(s, expires, now)).toBe(false)
    expect(pollInterval('new', expires, Date.parse(expires) + 1)).toBe(false)
  })

  it('short-polls the code and shows the Telegram account once it confirmed in the bot', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true })
    const { get, state } = mount(code())
    expect(screen.getByText(/Open this link in Telegram/)).toBeInTheDocument()
    const reads = () => get.mock.calls.filter((c) => String(c[0]).includes('link-codes')).length
    const before = reads()
    state.current = code({ state: 'telegram_confirmed', telegram: { id: 77, name: 'Eve', username: 'eve' } })
    await act(async () => { await vi.advanceTimersByTimeAsync(POLL_MS + 100) })
    await waitFor(() => expect(screen.getByText('Eve')).toBeInTheDocument())
    expect(reads()).toBeGreaterThan(before)
    expect(screen.getByText(/@eve, id 77\) wants to link/)).toBeInTheDocument()
  })
})

describe('confirm page', () => {
  const confirmed = code({ state: 'telegram_confirmed', telegram: { id: 77, name: 'Eve', username: 'eve' } })

  it("pre-fills the browser's time zone when the user has none and sends it with Confirm", async () => {
    const { onDone } = mount(confirmed, '')
    const post = vi.spyOn(api, 'POST').mockResolvedValue(answer({ id: 'tgl_1', timezone_saved: true }))
    expect(await screen.findByText(/Europe\/Vilnius/)).toBeInTheDocument()
    expect(screen.getByText(/from your browser, saved because you have none/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'change' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(onDone).toHaveBeenCalled())
    expect(post).toHaveBeenCalledWith('/me/telegram/link-codes/{code_id}/confirm', { params: { path: { code_id: 'cid' } }, body: { timezone: 'Europe/Vilnius' } })
  })

  it('shows and sends the stored zone, not the browser one, when the user chose one', async () => {
    mount(confirmed, 'Asia/Tokyo')
    const post = vi.spyOn(api, 'POST').mockResolvedValue(answer({ id: 'tgl_1' }))
    expect(await screen.findByText(/Asia\/Tokyo/)).toBeInTheDocument()
    expect(screen.queryByText(/from your browser/)).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }))
    await waitFor(() => expect(post).toHaveBeenCalled())
    expect(post.mock.calls[0]?.[1]).toMatchObject({ body: { timezone: 'Asia/Tokyo' } })
  })

  it('warns that the code leaked when another Telegram account used it', () => {
    const { onDone } = mount(code({ state: 'cancelled', reuse_attempted: true }))
    expect(screen.getByText('This code was used by someone else')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Confirm' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(onDone).toHaveBeenCalled()
  })

  it('says a plainly cancelled or expired code linked nothing', () => {
    mount(code({ state: 'expired' }))
    expect(screen.getByText('The code expired')).toBeInTheDocument()
  })
})
