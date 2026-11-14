import type { AttachmentsHost } from '@/components/attachmentsHost'
import { AttachmentBlobs } from './attachments'
import { downloadAttachment } from './downloads'
import { webApp } from './webapp'

/** Images load when they come within this distance of the viewport. */
const MARGIN = '300px 0px'

/**
 * The Mini App's AttachmentsHost over a blob store. `watch` gates a rendered body's images on visibility (an
 * IntersectionObserver; no observer, no gate: they load at once) and routes link clicks: an attachment link mints a
 * download, an external link opens through Telegram, an in-page anchor is left alone.
 */
export function createAttachmentsHost(blobs: AttachmentBlobs = new AttachmentBlobs()): AttachmentsHost & { blobs: AttachmentBlobs } {
  return {
    blobs,
    resolver: (spaceId) => blobs.resolver(spaceId),
    watch(root) {
      const waiting = new Map<Element, { ref: ReturnType<typeof refOf>; cb: (u: string | null) => void }>()
      function refOf(img: HTMLImageElement) {
        return blobs.refOf(img.getAttribute('src') ?? '')
      }
      const show = (img: HTMLImageElement, url: string | null) => {
        if (url && img.isConnected && img.getAttribute('src') !== url) img.setAttribute('src', url)
      }
      const start = (img: HTMLImageElement) => {
        const ref = refOf(img)
        if (!ref) return
        const cb = (u: string | null) => show(img, u)
        waiting.set(img, { ref, cb })
        void blobs.load(ref).then(cb)
      }
      const io =
        typeof IntersectionObserver === 'undefined'
          ? null
          : new IntersectionObserver(
              (items) => {
                for (const it of items) {
                  const img = it.target as HTMLImageElement
                  if (it.isIntersecting) start(img)
                  else {
                    const w = waiting.get(img)
                    if (w?.ref) blobs.cancel(w.ref, w.cb)
                    waiting.delete(img)
                  }
                }
              },
              { rootMargin: MARGIN },
            )
      for (const img of Array.from(root.querySelectorAll('img'))) {
        if (!refOf(img)) continue
        if (io) io.observe(img)
        else start(img)
      }
      const onClick = (ev: MouseEvent) => {
        const a = (ev.target as Element | null)?.closest?.('a')
        if (!a || !root.contains(a)) return
        const href = a.getAttribute('href') ?? ''
        const ref = blobs.refOf(href)
        if (ref) {
          ev.preventDefault()
          void downloadAttachment(ref, a.textContent?.trim().slice(0, 120) || undefined)
        } else if (/^https?:\/\//i.test(href)) {
          const app = webApp()
          if (app?.openLink) {
            ev.preventDefault()
            app.openLink(href)
          }
        }
      }
      root.addEventListener('click', onClick)
      return () => {
        io?.disconnect()
        root.removeEventListener('click', onClick)
        for (const w of waiting.values()) if (w.ref) blobs.cancel(w.ref, w.cb)
        waiting.clear()
      }
    },
  }
}
