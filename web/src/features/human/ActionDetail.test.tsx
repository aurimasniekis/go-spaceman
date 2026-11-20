import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { HumanAction } from '@/api/client'

const get = vi.fn()
vi.mock('@/api/queries', async (orig) => ({ ...(await orig<object>()), useMe: () => ({ data: { root: true, user: { id: 'usr_me' } } }) }))
vi.mock('@/api/session', () => ({ isMiniApp: () => false }))
vi.mock('@/api/client', () => ({ api: { GET: (...a: unknown[]) => get(...a), POST: () => Promise.resolve({}) }, ifMatch: () => '', unwrap: (r: unknown) => r }))
vi.mock('@/lib/notify', () => ({ notifyError: () => {}, notifyOk: () => {} }))
vi.mock('@/features/notifications/notify', () => ({ markOwnChange: () => {} }))
vi.mock('./EnforcedApproval', () => ({ useActionApproval: () => ({ data: undefined }), EnforcedApprovalCard: () => null }))
vi.mock('./RespondForm', () => ({ RespondForm: () => null }))
vi.mock('@/components/MarkdownView', () => ({ MarkdownView: ({ source }: { source: string }) => <div>{source}</div> }))
vi.mock('@/components/AttachmentList', () => ({ AttachmentList: () => null }))
vi.mock('@/components/AnchorLink', () => ({ AnchorLink: () => null }))
// The composer is TipTap; the test stands in for a successful post.
vi.mock('./CommentBox', () => ({
  CommentBox: ({ action, onDone }: { action: HumanAction; onDone: (a: HumanAction) => void }) => (
    <button onClick={() => onDone(withComments(action, 2, ['first', 'mine']))}>post</button>
  ),
}))

import { applyChange, INVALIDATE_WINDOW_MS } from '@/api/watch'
import { ActionDetail } from './ActionDetail'

const base = { id: 'hum_1', space_id: 'spc_a', title: 'Q', priority: 'normal', type: 'question', status: 'open', created_at: '2026-11-01T00:00:00Z', requester_kind: 'agent', reminder_count: 0 }
const withComments = (a: HumanAction, version: number, bodies: string[]): HumanAction =>
  ({ ...a, version, comments: bodies.map((b, i) => ({ id: `c${i}`, body: b, kind: 'comment', author: 'x', created_at: '2026-11-01T00:00:00Z' })) }) as unknown as HumanAction
const listed = { ...base, version: 1 } as unknown as HumanAction

const renderDetail = (qc: QueryClient) => render(
  <QueryClientProvider client={qc}><MantineProvider><ActionDetail action={listed} /></MantineProvider></QueryClientProvider>,
)

beforeEach(() => {
  get.mockReset()
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

describe('ActionDetail comments stay live', () => {
  it('shows a posted comment at once, without the list version having to change', async () => {
    get.mockResolvedValue(withComments(listed, 1, ['first']))
    renderDetail(new QueryClient({ defaultOptions: { queries: { retry: false } } }))
    await screen.findByText('first')
    get.mockResolvedValue(withComments(listed, 2, ['first', 'mine']))
    fireEvent.click(screen.getByText('post'))
    await screen.findByText('mine')
  })

  it("shows another's comment when a human_action change arrives on the change feed", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    get.mockResolvedValue(withComments(listed, 1, ['first']))
    renderDetail(qc)
    await screen.findByText('first')
    get.mockResolvedValue(withComments(listed, 2, ['first', "agent's reply"]))
    applyChange(qc, { seq: 1, at: 0, space_id: 'spc_a', resource_type: 'human_action', resource_id: 'hum_1', action: 'updated' } as never)
    await waitFor(() => expect(screen.getByText("agent's reply")).toBeTruthy(), { timeout: INVALIDATE_WINDOW_MS + 2000 })
  })
})
