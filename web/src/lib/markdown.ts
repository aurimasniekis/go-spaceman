import DOMPurify, { type DOMPurify as Purifier } from 'dompurify'
import { Marked, Renderer, type Tokens } from 'marked'
import markedFootnote from 'marked-footnote'
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

/**
 * Turns an attachment ID into the URL an <img> or link loads. The main web app uses the content URL, which the session
 * cookie authorizes; another transport (for example a Telegram Mini App with blob: URLs) passes its own.
 */
export type AttachmentResolver = (id: string) => string

/** The web app's resolver: the attachment's content URL in the space. */
export const spaceAttachments = (spaceId: string): AttachmentResolver => (id) => attachmentURL(spaceId, id)

/** Limits that keep a hostile body from freezing the page (the enhancers in components/markdown read them too). */
export const MARKDOWN_LIMITS = {
  /** Mermaid diagrams rendered per body; later ones show their source. */
  diagrams: 20,
  /** Characters of one diagram's source (mermaid's maxTextSize). */
  diagramSource: 10_000,
  /** Edges of one diagram (mermaid's maxEdges). */
  diagramEdges: 500,
  /** Highlighted code blocks per body; later ones stay plain. */
  codeBlocks: 50,
  /** Characters of one highlighted code block; longer ones stay plain. */
  codeLength: 20_000,
  /** Columns of leading indentation outside fenced code (marked slows down steeply with list nesting depth). */
  indent: 64,
  /** Leading block markers (>, -, 1.) on one line (the DOM depth they produce slows sanitizing down quadratically). */
  markers: 16,
  /**
   * Emphasis delimiters (*, _, ~) in one paragraph; beyond this they are escaped (shown literally). marked matches them
   * in quadratic time: 8,000 unmatched "*a " took 15 seconds.
   */
  delimiters: 256,
} as const

const MARKER = '[ \\t]*(?:>[ \\t]?|(?:[-*+]|\\d{1,9}[.)])[ \\t]+)'
const EXCESS_MARKERS = new RegExp(`^((?:${MARKER}){${MARKDOWN_LIMITS.markers}})(?:${MARKER})+`)

/** Escapes the emphasis delimiters of a paragraph, leaving code spans and existing escapes alone. */
function escapeDelimiters(block: string): string {
  return block.replace(/\\[\s\S]|(`+)[\s\S]*?\1|[*_~]/g, (m) => (m.length === 1 ? `\\${m}` : m))
}

/**
 * Bounds the work a hostile body can cause, outside fenced code: leading indentation and stacked block markers are
 * capped (nesting depth), and a paragraph with too many emphasis delimiters has them escaped. Real bodies are nowhere
 * near the limits; unbounded, 2,000 nested list levels took marked minutes.
 */
export function limitNesting(md: string): string {
  const out: string[] = []
  let block: string[] = []
  const flush = () => {
    if (!block.length) return
    const text = block.join('\n')
    out.push((text.match(/[*_~]/g)?.length ?? 0) > MARKDOWN_LIMITS.delimiters ? escapeDelimiters(text) : text)
    block = []
  }
  let fence = ''
  for (let line of md.split('\n')) {
    const f = line.match(/^\s*(`{3,}|~{3,})/)
    if (fence) {
      if (f && f[1][0] === fence[0] && f[1].length >= fence.length && line.trim() === f[1]) fence = ''
      out.push(line)
      continue
    }
    if (f) {
      flush()
      fence = f[1]
      out.push(line)
      continue
    }
    if (!line.trim()) {
      flush()
      out.push(line)
      continue
    }
    const ws = line.match(/^[ \t]*/)![0]
    let cols = 0
    for (const c of ws) cols = c === '\t' ? cols + 4 - (cols % 4) : cols + 1
    if (cols > MARKDOWN_LIMITS.indent) line = ' '.repeat(MARKDOWN_LIMITS.indent) + line.slice(ws.length)
    block.push(line.replace(EXCESS_MARKERS, '$1'))
  }
  flush()
  return out.join('\n')
}

const ATTACHMENT_SRC = new RegExp(`^${API_PREFIX}/spaces/[^/?#]+/attachments/${ID}/content$`)
const FORBID_TAGS = ['style', 'form', 'iframe', 'frame', 'object', 'embed', 'svg', 'math', 'button', 'textarea', 'select', 'link', 'meta', 'base']

/** State of the one render in progress (rendering is synchronous, so module state is safe). */
const current = { prefix: 'user-content-', slugs: new Map<string, number>(), resolve: ((id: string) => id) as AttachmentResolver, resolved: new Set<string>() }

const escapeHTML = (s: string) => s.replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]!)

/** GitHub-style heading slugs, unique within one body. */
function slug(text: string): string {
  const base = text.toLowerCase().replace(/<[^>]*>/g, '').replace(/[^\p{L}\p{N}\s_-]/gu, '').trim().replace(/\s+/g, '-').slice(0, 80) || 'section'
  const n = current.slugs.get(base) ?? 0
  current.slugs.set(base, n + 1)
  return n ? `${base}-${n}` : base
}

/** A fence's language: its first word, restricted to safe characters (it becomes a class and a data attribute). */
function fenceLang(lang?: string): string {
  return (lang ?? '').trim().split(/\s+/)[0].toLowerCase().replace(/[^a-z0-9_+#.-]/g, '').slice(0, 32)
}

const marked = new Marked({ gfm: true, breaks: true, async: false })
marked.use(markedFootnote({ prefixId: 'sm-fn-', description: 'Footnotes' }))
marked.use({
  renderer: {
    code({ text, lang }: Tokens.Code) {
      const l = fenceLang(lang)
      if (l === 'mermaid') {
        // Rendered by components/markdown/MermaidDiagram once mounted; until then (and without it) the source shows.
        return `<div class="sm-mermaid" data-sm-mermaid=""><pre tabindex="0"><code>${escapeHTML(text)}</code></pre></div>\n`
      }
      const cls = l ? ` class="language-${l}" data-lang="${l}"` : ''
      return `<div class="sm-codeblock"><pre tabindex="0"><code${cls}>${escapeHTML(text)}</code></pre></div>\n`
    },
    heading({ tokens, depth, text }: Tokens.Heading) {
      const id = current.prefix + slug(text)
      const inner = this.parser.parseInline(tokens)
      return `<h${depth} id="${id}">${inner}<a class="sm-anchor" href="#${id}" aria-label="Link to this section">#</a></h${depth}>\n`
    },
    table(token: Tokens.Table) {
      // Wide tables scroll inside their block (no page overflow at 390 px); the block is focusable for keyboard scrolling.
      return `<div class="sm-table" tabindex="0">${Renderer.prototype.table.call(this, token)}</div>\n`
    },
  },
  hooks: {
    // Footnote IDs get the view's prefix too, so two bodies on one page don't share IDs.
    postprocess: (html: string) => html.replace(/(id="|href="#|aria-describedby=")sm-fn-/g, (_, a: string) => `${a}${current.prefix}fn-`),
  },
})

let purifier: Purifier | undefined

/**
 * A DOMPurify instance of its own: its hooks must not touch other users of DOMPurify (Mermaid sanitizes its labels and
 * SVG with the shared instance, and would get target="_blank" or rewritten images otherwise).
 */
function purify(): Purifier {
  if (purifier) return purifier
  const p = DOMPurify(window)
  p.addHook('beforeSanitizeAttributes', (node) => {
    // Attachment references become the resolver's URL (DOMPurify would drop the unknown spaceman: scheme).
    for (const attr of ['src', 'href']) {
      const v = node.getAttribute?.(attr)
      const m = v?.match(new RegExp(`^spaceman://attachment/${ID}$`))
      if (m) {
        const url = current.resolve(m[1])
        current.resolved.add(url)
        node.setAttribute(attr, url)
      }
    }
  })
  p.addHook('afterSanitizeAttributes', (node) => {
    if (node.tagName === 'A') {
      const href = node.getAttribute('href') ?? ''
      // In-page links (heading anchors, footnotes) stay in the tab; everything else opens a new one without a referrer.
      if (!href.startsWith('#')) {
        node.setAttribute('target', '_blank')
        node.setAttribute('rel', 'noopener noreferrer nofollow')
      }
    }
    // Only attachment images render; anything else (tracking pixels, remote content, data: URLs) becomes a link or text.
    if (node.tagName === 'IMG') {
      const src = node.getAttribute('src') ?? ''
      if (!current.resolved.has(src) && !ATTACHMENT_SRC.test(src)) {
        const alt = node.getAttribute('alt') || src
        if (/^https?:\/\//i.test(src)) {
          const a = node.ownerDocument.createElement('a')
          a.setAttribute('href', src)
          a.setAttribute('target', '_blank')
          a.setAttribute('rel', 'noopener noreferrer nofollow')
          a.textContent = alt
          node.replaceWith(a)
        } else node.replaceWith(node.ownerDocument.createTextNode(alt))
      } else {
        node.setAttribute('loading', 'lazy')
        node.setAttribute('decoding', 'async')
      }
    }
    if (node.tagName === 'INPUT') {
      // Task-list checkboxes are read-only and get their item's text as their name (axe label); no other inputs.
      if (node.getAttribute('type') !== 'checkbox') node.remove()
      else {
        node.setAttribute('disabled', '')
        node.setAttribute('aria-label', (node.parentElement?.textContent ?? '').trim().slice(0, 200) || 'Task item')
      }
    }
  })
  purifier = p
  return p
}

export interface RenderOptions {
  /** How attachment references load (default: the space's content URLs). */
  resolveAttachment?: AttachmentResolver
  /** Prefix of heading and footnote IDs; unique per view on a page (default "user-content-"). */
  idPrefix?: string
}

/**
 * Renders untrusted Markdown to sanitized HTML (SM-ATT-003, SM-UI-005): GFM (tables, task lists, strikethrough,
 * autolinks, footnotes, heading anchors), attachment references resolved, code blocks marked for highlighting and
 * Mermaid fences marked for diagrams (components/markdown does both, lazily). No scripts, styles, event handlers,
 * frames, forms, javascript: URLs or remote images.
 */
export function renderMarkdown(md: string, spaceId: string, opts: RenderOptions = {}): string {
  current.prefix = (opts.idPrefix ?? 'user-content-').replace(/[^A-Za-z0-9_-]/g, '')
  current.slugs = new Map()
  current.resolve = opts.resolveAttachment ?? spaceAttachments(spaceId)
  current.resolved = new Set()
  let html: string
  try {
    html = marked.parse(limitNesting(md)) as string
  } catch {
    // A pathological body (for example nesting deep enough to exhaust the parser's stack) shows as plain text.
    html = `<pre tabindex="0">${escapeHTML(md)}</pre>`
  }
  return purify().sanitize(html, { FORBID_TAGS, FORBID_ATTR: ['style'] })
}
