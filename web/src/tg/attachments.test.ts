import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import * as session from '@/api/session'
import { AttachmentBlobs } from './attachments'
import * as tg from './session'
import { createAttachmentsHost } from './watchAttachments'

let n = 0
beforeEach(() => {
  n = 0
  vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: () => `blob:test/${++n}`, revokeObjectURL: vi.fn() }))
  session.setTelegramSession('tok')
})
afterEach(() => vi.restoreAllMocks())

const img = (type = 'image/png') => new Response(new Blob(['x'], { type }), { status: 200, headers: { 'Content-Type': type } })
const A = (id: string) => ({ spaceId: 'spc_1', id })

describe('AttachmentBlobs', () => {
  it('fetches with the Telegram header and no cookie, at most `concurrency` at a time', async () => {
    const gates: Array<() => void> = []
    let running = 0
    let peak = 0
    const f = vi.fn(async (_u: string, _i: RequestInit) => {
      running++
      peak = Math.max(peak, running)
      await new Promise<void>((r) => gates.push(r))
      running--
      return img()
    })
    const b = new AttachmentBlobs(f as unknown as typeof fetch, { concurrency: 2 })
    const all = ['att_a', 'att_b', 'att_c', 'att_d'].map((id) => b.load(A(id)))
    await vi.waitFor(() => expect(gates.length).toBe(2))
    expect(f).toHaveBeenCalledTimes(2)
    const init = f.mock.calls[0][1]
    expect(init.credentials).toBe('omit')
    expect(init.headers).toEqual({ Authorization: 'Telegram tok' })
    expect(f.mock.calls[0][0]).toBe('/api/v1/spaces/spc_1/attachments/att_a/content')
    while (gates.length) {
      gates.shift()!()
      await new Promise((r) => setTimeout(r))
    }
    const urls = await Promise.all(all)
    expect(urls.every((u) => u?.startsWith('blob:'))).toBe(true)
    expect(peak).toBe(2)
  })

  it('fetches at most 3 at a time by default', async () => {
    const gates: Array<() => void> = []
    let running = 0
    let peak = 0
    const f = vi.fn(async () => {
      running++
      peak = Math.max(peak, running)
      await new Promise<void>((r) => gates.push(r))
      running--
      return img()
    })
    const b = new AttachmentBlobs(f as unknown as typeof fetch)
    const all = ['a', 'b', 'c', 'd', 'e'].map((id) => b.load(A(`att_${id}`)))
    await vi.waitFor(() => expect(gates.length).toBe(3))
    await new Promise((r) => setTimeout(r))
    expect(f).toHaveBeenCalledTimes(3)
    while (gates.length) {
      gates.shift()!()
      await new Promise((r) => setTimeout(r))
    }
    await Promise.all(all)
    expect(peak).toBe(3)
  })

  it('resolves to a placeholder until loaded, then to the blob', async () => {
    const b = new AttachmentBlobs((async () => img()) as unknown as typeof fetch)
    const r = b.resolver('spc_1')
    const first = r('att_a')
    expect(first.startsWith('blob:')).toBe(true)
    expect(b.refOf(first)).toEqual(A('att_a'))
    const url = await b.load(A('att_a'))
    expect(url).not.toBe(first)
    expect(r('att_a')).toBe(url)
    expect(b.resolver('spc_1')).toBe(r)
  })

  it('refuses a body that is not a raster image, and an oversized one', async () => {
    const html = new AttachmentBlobs((async () => img('text/html')) as unknown as typeof fetch)
    expect(await html.load(A('att_a'))).toBeNull()
    const svg = new AttachmentBlobs((async () => img('image/svg+xml')) as unknown as typeof fetch)
    expect(await svg.load(A('att_a'))).toBeNull()
    const big = new AttachmentBlobs((async () => new Response('x', { headers: { 'Content-Type': 'image/png', 'Content-Length': '999' } })) as unknown as typeof fetch, { maxBytes: 10 })
    expect(await big.load(A('att_a'))).toBeNull()
  })

  it('does not retry a failure at once, and ends the session on 401', async () => {
    const f = vi.fn(async () => new Response('no', { status: 404 }))
    const b = new AttachmentBlobs(f as unknown as typeof fetch)
    expect(await b.load(A('att_a'))).toBeNull()
    expect(await b.load(A('att_a'))).toBeNull()
    expect(f).toHaveBeenCalledOnce()
    const end = vi.spyOn(tg, 'endSession')
    const u = new AttachmentBlobs((async () => new Response('', { status: 401 })) as unknown as typeof fetch)
    expect(await u.load(A('att_b'))).toBeNull()
    expect(end).toHaveBeenCalled()
  })

  it('withdraws a queued request that left the viewport', async () => {
    let release: () => void = () => undefined
    const f = vi.fn(async () => {
      await new Promise<void>((r) => (release = r))
      return img()
    })
    const b = new AttachmentBlobs(f as unknown as typeof fetch, { concurrency: 1 })
    void b.load(A('att_a'))
    const queued = b.load(A('att_b'))
    b.cancel(A('att_b'))
    expect(await queued).toBeNull()
    release()
    await vi.waitFor(() => expect(f).toHaveBeenCalledTimes(1))
  })
})

describe('createAttachmentsHost.watch', () => {
  it('loads an image only once it is visible, and swaps in the blob', async () => {
    const observers: Array<{ cb: IntersectionObserverCallback; els: Element[] }> = []
    const disconnect = vi.fn()
    vi.stubGlobal(
      'IntersectionObserver',
      class {
        els: Element[] = []
        constructor(public cb: IntersectionObserverCallback) {
          observers.push(this)
        }
        observe(e: Element) {
          this.els.push(e)
        }
        disconnect = disconnect
      },
    )
    const f = vi.fn(async () => img())
    const host = createAttachmentsHost(new AttachmentBlobs(f as unknown as typeof fetch))
    const src = host.resolver('spc_1')('att_a')
    const root = document.createElement('div')
    root.innerHTML = `<img src="${src}"><img src="https://example.com/x.png">`
    document.body.append(root)
    const stop = host.watch(root)
    expect(observers[0].els).toHaveLength(1)
    expect(f).not.toHaveBeenCalled()
    const el = observers[0].els[0] as HTMLImageElement
    observers[0].cb([{ target: el, isIntersecting: true } as unknown as IntersectionObserverEntry], observers[0] as unknown as IntersectionObserver)
    await vi.waitFor(() => expect(el.getAttribute('src')).not.toBe(src))
    expect(el.getAttribute('src')).toMatch(/^blob:/)
    expect(disconnect).not.toHaveBeenCalled()
    stop()
    expect(disconnect).toHaveBeenCalledOnce()
    root.remove()
  })

  it('turns a click on an attachment link into a download instead of following it', async () => {
    const dl = await import('./downloads')
    const spy = vi.spyOn(dl, 'downloadAttachment').mockResolvedValue()
    const host = createAttachmentsHost(new AttachmentBlobs((async () => img()) as unknown as typeof fetch))
    const href = host.resolver('spc_1')('att_a')
    const root = document.createElement('div')
    root.innerHTML = `<a href="${href}">report.pdf</a>`
    document.body.append(root)
    const stop = host.watch(root)
    const ev = new MouseEvent('click', { bubbles: true, cancelable: true })
    root.querySelector('a')!.dispatchEvent(ev)
    expect(ev.defaultPrevented).toBe(true)
    expect(spy).toHaveBeenCalledWith(A('att_a'), 'report.pdf')
    stop()
    root.remove()
  })
})

describe('eviction', () => {
  it('keeps the newest, revokes the oldest and tells listeners so a displayed image can reload', async () => {
    const b = new AttachmentBlobs((async () => img()) as unknown as typeof fetch, { maxCached: 2 })
    const evicted: string[] = []
    b.onEvict((r) => evicted.push(r.id))
    const urls = []
    for (const id of ['att_a', 'att_b', 'att_c']) urls.push(await b.load(A(id)))
    expect(evicted).toEqual(['att_a'])
    // The evicted blob is released, and only it.
    expect(URL.revokeObjectURL).toHaveBeenCalledWith(urls[0])
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith(urls[1])
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith(urls[2])
    expect(b.resolver('spc_1')('att_c')).toBe(urls[2])
    expect(b.resolver('spc_1')('att_a')).not.toBe(urls[0])
    expect(await b.load(A('att_a'))).toMatch(/^blob:/)
  })

  it('also caps the bytes kept: the least recently loaded are revoked until the total fits, the newest always stays', async () => {
    const sized = (n: number) => new Response('x'.repeat(n), { status: 200, headers: { 'Content-Type': 'image/png' } })
    const sizes: Record<string, number> = { att_a: 40, att_b: 40, att_c: 40, att_d: 500 }
    const f = vi.fn(async (u: string) => sized(sizes[u.split('/').at(-2)!]))
    const b = new AttachmentBlobs(f as unknown as typeof fetch, { maxTotalBytes: 100, maxBytes: 1000 })
    const evicted: string[] = []
    b.onEvict((r) => evicted.push(r.id))
    const urls: Record<string, string | null> = {}
    for (const id of ['att_a', 'att_b']) urls[id] = await b.load(A(id))
    expect(evicted).toEqual([])
    urls.att_c = await b.load(A('att_c')) // 120 > 100: the oldest goes
    expect(evicted).toEqual(['att_a'])
    expect(URL.revokeObjectURL).toHaveBeenCalledWith(urls.att_a)
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith(urls.att_b)
    urls.att_d = await b.load(A('att_d')) // alone over the cap: everything older goes, the newest stays
    expect(evicted).toEqual(['att_a', 'att_b', 'att_c'])
    expect(b.resolver('spc_1')('att_d')).toBe(urls.att_d)
    expect(URL.revokeObjectURL).not.toHaveBeenCalledWith(urls.att_d)
  })
})
