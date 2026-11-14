import { Box } from '@mantine/core'
import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { type AttachmentResolver, type DiagramPlan, MARKDOWN_LIMITS, planDiagrams, renderMarkdown } from '@/lib/markdown'
import { enhanceCode } from './markdown/enhance'
import { MermaidDiagram } from './markdown/MermaidDiagram'
import './composer/composer.css'
import './markdown/markdown.css'

/** FNV-1a of a string, in base 36: short and the same on every load. */
function hash(s: string): string {
  let h = 0x811c9dc5
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 0x01000193)
  return (h >>> 0).toString(36)
}

/** Prefixes in use on the page, per body hash: the slots taken by mounted views. */
const prefixSlots = new Map<string, Set<number>>()

/**
 * The prefix of a body's heading and footnote IDs: the same on every load (from the body's text), so a link to a
 * heading still works after a reload or in a new tab. A second view of the same body on the page gets a numbered prefix
 * of its own once mounted, so no two views share an ID.
 */
function useAnchorPrefix(source: string): string {
  const h = useMemo(() => hash(source), [source])
  const [slot, setSlot] = useState(0)
  useEffect(() => {
    const taken = prefixSlots.get(h) ?? new Set<number>()
    prefixSlots.set(h, taken)
    let n = 0
    while (taken.has(n)) n++
    taken.add(n)
    setSlot(n)
    return () => {
      taken.delete(n)
      if (!taken.size) prefixSlots.delete(h)
    }
  }, [h])
  return `user-content-${h}${slot ? `-${slot}` : ''}-`
}

const safeDecode = (s: string) => {
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

type Diagram = { el: HTMLElement; source: string; plan: DiagramPlan }

/**
 * Displays untrusted Markdown, sanitized, with the space's attachments resolved (SM-ATT-003, SM-UI-005): GFM, heading
 * anchors and footnotes, highlighted code blocks with a copy button, and Mermaid diagrams. The highlighter and mermaid
 * load lazily, only for bodies that need them.
 */
export function MarkdownView({ source, spaceId, resolveAttachment }: { source: string; spaceId: string; resolveAttachment?: AttachmentResolver }) {
  const idPrefix = useAnchorPrefix(source)
  const html = useMemo(() => renderMarkdown(source, spaceId, { resolveAttachment, idPrefix }), [source, spaceId, resolveAttachment, idPrefix])
  const ref = useRef<HTMLDivElement>(null)
  const [diagrams, setDiagrams] = useState<Diagram[]>([])
  const scrolled = useRef(false)

  useEffect(() => {
    const root = ref.current
    if (!root) return
    const stop = enhanceCode(root)
    // Mermaid placeholders keep their source (shown until the diagram mounts, and when there are too many diagrams).
    const els = Array.from(root.querySelectorAll<HTMLElement>('[data-sm-mermaid]')).slice(0, MARKDOWN_LIMITS.diagrams)
    const sources = els.map((el) => el.querySelector('pre')?.textContent ?? '')
    // The body's diagrams share one budget: small ones render on their own, large ones after a click.
    const plans = planDiagrams(sources)
    const found = els.map((el, i) => {
      el.setAttribute('data-sm-mounted', '')
      return { el, source: sources[i], plan: plans[i] }
    })
    setDiagrams((prev) => (prev.length || found.length ? found : prev))
    // A link to one of this body's headings or footnotes (opened in a new tab, or the page reloaded): the body renders
    // after the browser looked for the target, so scroll to it now.
    const target = safeDecode(window.location.hash.slice(1))
    if (!scrolled.current && target.startsWith(idPrefix)) {
      const el = root.ownerDocument.getElementById(target)
      if (el && root.contains(el)) {
        scrolled.current = true
        el.scrollIntoView?.()
      }
    }
    return stop
  }, [html, idPrefix])

  return (
    <>
      {/* The HTML is produced by marked and sanitized by DOMPurify (scripts, handlers, styles and remote images removed). */}
      <Box ref={ref} className="sm-markdown" fz="md" dangerouslySetInnerHTML={{ __html: html }} />
      {diagrams.map((d, i) => createPortal(<MermaidDiagram source={d.source} plan={d.plan} />, d.el, `${i}`))}
    </>
  )
}
