import { afterEach, describe, expect, it, vi } from 'vitest'

const post = vi.fn()
vi.mock('@/api/client', () => ({ api: { POST: (...a: unknown[]) => post(...a) } }))
const show = vi.fn()
vi.mock('@mantine/notifications', () => ({ notifications: { show: (...a: unknown[]) => show(...a) } }))

import { downloadAttachment } from './downloads'

afterEach(() => {
  vi.clearAllMocks()
  delete (window as unknown as { Telegram?: unknown }).Telegram
})

const ref = { spaceId: 'spc_1', id: 'att_1' }

describe('downloadAttachment', () => {
  it('mints a link and hands it to Telegram', async () => {
    const downloadFile = vi.fn()
    ;(window as unknown as { Telegram: unknown }).Telegram = { WebApp: { downloadFile } }
    post.mockResolvedValue({ data: { url: 'https://tg.example.com/tg-dl/abc' }, response: new Response() })
    await downloadAttachment(ref, 'r.pdf')
    expect(post).toHaveBeenCalledWith('/telegram/download-links', { body: { space_id: 'spc_1', attachment_id: 'att_1' } })
    expect(downloadFile).toHaveBeenCalledWith({ url: 'https://tg.example.com/tg-dl/abc', file_name: 'r.pdf' })
  })
  it('answers a 429 with a notice that says when to retry, not an error screen', async () => {
    const downloadFile = vi.fn()
    ;(window as unknown as { Telegram: unknown }).Telegram = { WebApp: { downloadFile } }
    post.mockResolvedValue({ error: {}, response: new Response('', { status: 429, headers: { 'Retry-After': '5' } }) })
    await downloadAttachment(ref)
    expect(downloadFile).not.toHaveBeenCalled()
    expect(show).toHaveBeenCalledWith(expect.objectContaining({ message: expect.stringContaining('Try again in 5 s') }))
  })
  it('says so when the file cannot be downloaded, and stays quiet on a 401 (the session already ended)', async () => {
    post.mockResolvedValue({ error: {}, response: new Response('', { status: 404 }) })
    await downloadAttachment(ref)
    expect(show).toHaveBeenCalledOnce()
    show.mockClear()
    post.mockResolvedValue({ error: {}, response: new Response('', { status: 401 }) })
    await downloadAttachment(ref)
    expect(show).not.toHaveBeenCalled()
  })
})
