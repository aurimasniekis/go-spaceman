import { API_PREFIX, endMiniAppSession, miniAppFetchInit } from '@/api/session'
import type { AttachmentResolver } from '@/lib/markdown'

/**
 * The Mini App's attachments (plan §4.6). The session is a header, so an <img src> cannot carry it: an image is fetched
 * with Authorization into a blob: URL, and only once it is near the viewport (visibility gate), at most `concurrency`
 * at a time. Until then the image shows a placeholder blob. Links to attachments are not followed: a click mints a
 * signed download link (downloads.ts).
 */

const PLACEHOLDER_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="320" height="180" viewBox="0 0 320 180"><rect width="320" height="180" fill="#8884" /></svg>'

export interface AttachmentRef {
  spaceId: string
  id: string
}

export interface BlobOptions {
  concurrency?: number
  /** Largest image fetched (bytes): a larger Content-Length or body is not shown. */
  maxBytes?: number
  /** Blob URLs kept (oldest revoked first). */
  maxCached?: number
  /** Total bytes of the blobs kept (oldest revoked first); the newest is always kept. */
  maxTotalBytes?: number
  /** Failed fetches are retried no sooner than this (ms). */
  retryAfterMs?: number
}

type Entry = {
  ref: AttachmentRef
  placeholder: string
  blob?: string
  size?: number
  state: 'idle' | 'queued' | 'loading' | 'done' | 'failed'
  failedAt?: number
  waiters: Set<(url: string | null) => void>
}

export class AttachmentBlobs {
  private readonly entries = new Map<string, Entry>()
  private readonly byPlaceholder = new Map<string, Entry>()
  private readonly queue: Entry[] = []
  private active = 0
  private readonly resolvers = new Map<string, AttachmentResolver>()
  private readonly evictListeners = new Set<(ref: AttachmentRef) => void>()
  private readonly o: Required<BlobOptions>

  constructor(
    private readonly doFetch: typeof fetch = (...a) => fetch(...a),
    o: BlobOptions = {},
  ) {
    this.o = { concurrency: 3, maxBytes: 16 << 20, maxCached: 64, maxTotalBytes: 96 << 20, retryAfterMs: 15_000, ...o }
  }

  private entry(ref: AttachmentRef): Entry {
    const key = `${ref.spaceId}/${ref.id}`
    let e = this.entries.get(key)
    if (!e) {
      const placeholder = URL.createObjectURL(new Blob([PLACEHOLDER_SVG], { type: 'image/svg+xml' }))
      e = { ref, placeholder, state: 'idle', waiters: new Set() }
      this.entries.set(key, e)
      this.byPlaceholder.set(placeholder, e)
    }
    return e
  }

  /** The Markdown renderer's resolver for a space: the loaded blob, else the entry's placeholder. Stable per space. */
  resolver(spaceId: string): AttachmentResolver {
    let r = this.resolvers.get(spaceId)
    if (!r) {
      r = (id) => {
        const e = this.entry({ spaceId, id })
        return e.blob ?? e.placeholder
      }
      this.resolvers.set(spaceId, r)
    }
    return r
  }

  /** The attachment a placeholder or loaded blob URL stands for, if it is one of ours. */
  refOf(url: string): AttachmentRef | undefined {
    const e = this.byPlaceholder.get(url) ?? [...this.entries.values()].find((x) => x.blob === url)
    return e?.ref
  }

  /** Asks for the image (queued behind the concurrency cap). Resolves with its blob URL, or null on failure or cancel. */
  load(ref: AttachmentRef): Promise<string | null> {
    const e = this.entry(ref)
    if (e.state === 'done') return Promise.resolve(e.blob ?? null)
    if (e.state === 'failed' && Date.now() - (e.failedAt ?? 0) < this.o.retryAfterMs) return Promise.resolve(null)
    return new Promise((resolve) => {
      e.waiters.add(resolve)
      if (e.state === 'idle' || e.state === 'failed') {
        e.state = 'queued'
        this.queue.push(e)
        this.pump()
      }
    })
  }

  /** Withdraws a request that has not started (the image left the viewport). A started fetch finishes. */
  cancel(ref: AttachmentRef, waiter?: (url: string | null) => void) {
    const e = this.entries.get(`${ref.spaceId}/${ref.id}`)
    if (!e || e.state !== 'queued') return
    if (waiter) e.waiters.delete(waiter)
    if (e.waiters.size > 0 && waiter) return
    const i = this.queue.indexOf(e)
    if (i >= 0) this.queue.splice(i, 1)
    e.state = 'idle'
    for (const w of e.waiters) w(null)
    e.waiters.clear()
  }

  private pump() {
    while (this.active < this.o.concurrency && this.queue.length > 0) {
      const e = this.queue.shift()!
      e.state = 'loading'
      this.active++
      void this.fetchOne(e).finally(() => {
        this.active--
        this.pump()
      })
    }
  }

  private async fetchOne(e: Entry) {
    let url: string | null = null
    let size = 0
    try {
      const { spaceId, id } = e.ref
      const res = await this.doFetch(`${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/attachments/${encodeURIComponent(id)}/content`, miniAppFetchInit())
      if (res.status === 401) {
        await endMiniAppSession(res)
      } else if (res.ok) {
        const type = (res.headers.get('Content-Type') ?? '').split(';')[0].trim().toLowerCase()
        const len = Number(res.headers.get('Content-Length') ?? '0')
        if (/^image\/(png|jpeg|gif|webp|avif|bmp)$/.test(type) && !(len > this.o.maxBytes)) {
          const body = await res.blob()
          if (body.size <= this.o.maxBytes) {
            url = URL.createObjectURL(new Blob([body], { type }))
            size = body.size
          }
        } else {
          await res.body?.cancel()
        }
      } else {
        await res.body?.cancel()
      }
    } catch {
      url = null
    }
    if (url) {
      e.blob = url
      e.size = size
      e.state = 'done'
      const key = `${e.ref.spaceId}/${e.ref.id}`
      this.entries.delete(key)
      this.entries.set(key, e)
      this.evict()
    } else {
      e.state = 'failed'
      e.failedAt = Date.now()
    }
    for (const w of e.waiters) w(url)
    e.waiters.clear()
  }

  /** Told when a loaded image's blob is revoked, so a displayed one goes back to its placeholder and reloads when seen. */
  onEvict(cb: (ref: AttachmentRef) => void): () => void {
    this.evictListeners.add(cb)
    return () => this.evictListeners.delete(cb)
  }

  /**
   * Revokes the least recently loaded blobs while there are more than `maxCached` or they hold more than
   * `maxTotalBytes` (never the one that just loaded: it is the last).
   */
  private evict() {
    const loaded = [...this.entries.values()].filter((x) => x.blob)
    let count = loaded.length
    let bytes = loaded.reduce((n, x) => n + (x.size ?? 0), 0)
    for (const x of loaded.slice(0, -1)) {
      if (count <= this.o.maxCached && bytes <= this.o.maxTotalBytes) break
      URL.revokeObjectURL(x.blob!)
      count--
      bytes -= x.size ?? 0
      x.blob = undefined
      x.size = undefined
      x.state = 'idle'
      // Re-insert so a later load counts as the newest.
      const key = `${x.ref.spaceId}/${x.ref.id}`
      this.entries.delete(key)
      this.entries.set(key, x)
      for (const l of this.evictListeners) l(x.ref)
    }
  }

  /** Revokes every blob URL (the app is closing). */
  dispose() {
    for (const e of this.entries.values()) {
      URL.revokeObjectURL(e.placeholder)
      if (e.blob) URL.revokeObjectURL(e.blob)
    }
    this.entries.clear()
    this.byPlaceholder.clear()
    this.queue.length = 0
  }
}
