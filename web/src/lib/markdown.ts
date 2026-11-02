import DOMPurify from 'dompurify'
import { marked } from 'marked'
import { API_PREFIX } from '@/api/session'

/** Markdown bodies reference attachments as spaceman://attachment/<id> (SM-ATT-005). */
export const ATTACHMENT_URI = 'spaceman://attachment/'

const ID = '(att_[0-9a-z]{26})'

/** The content URL of an attachment in a space. */
export function attachmentURL(spaceId: string, id: string): string {
  return `${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/attachments/${id}/content`
}

/** Rewrites attachment references to content URLs of the space (for display and editing). */
export function resolveAttachments(md: string, spaceId: string): string {
  return md.replace(new RegExp(`spaceman://attachment/${ID}`, 'g'), (_, id: string) => attachmentURL(spaceId, id))
}

/** Rewrites content URLs of the space back to attachment references (what is stored). */
export function referenceAttachments(md: string, spaceId: string): string {
  const prefix = `${API_PREFIX}/spaces/${encodeURIComponent(spaceId)}/attachments/`.replace(/[.*+?^${}()|[\]\\/]/g, '\\$&')
  return md.replace(new RegExp(`(?:https?://[^/\\s)]+)?${prefix}${ID}/content`, 'g'), (_, id: string) => ATTACHMENT_URI + id)
}

/** The attachment IDs a Markdown body references. */
export function referencedAttachments(md: string): string[] {
  return [...new Set([...md.matchAll(new RegExp(`spaceman://attachment/${ID}`, 'g'))].map((m) => m[1]))]
}

let hooked = false

function hook() {
  if (hooked) return
  hooked = true
  DOMPurify.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName === 'A') {
      node.setAttribute('target', '_blank')
      node.setAttribute('rel', 'noopener noreferrer nofollow')
    }
    // Only same-origin attachment images render; anything else (tracking pixels, remote content) becomes a link.
    if (node.tagName === 'IMG') {
      const src = node.getAttribute('src') ?? ''
      if (!src.startsWith(`${API_PREFIX}/spaces/`)) {
        const a = node.ownerDocument.createElement('a')
        a.setAttribute('href', src)
        a.setAttribute('target', '_blank')
        a.setAttribute('rel', 'noopener noreferrer nofollow')
        a.textContent = node.getAttribute('alt') || src
        node.replaceWith(a)
      } else {
        node.setAttribute('loading', 'lazy')
      }
    }
  })
}

/**
 * Renders untrusted Markdown to sanitized HTML (SM-ATT-003, SM-UI-005): GFM, attachment references resolved to the
 * space, no scripts, styles, event handlers or remote images.
 */
export function renderMarkdown(md: string, spaceId: string): string {
  hook()
  const html = marked.parse(resolveAttachments(md, spaceId), { gfm: true, breaks: true, async: false })
  return DOMPurify.sanitize(html, { FORBID_TAGS: ['style', 'form', 'iframe', 'object', 'embed', 'svg', 'math'], FORBID_ATTR: ['style'] })
}
