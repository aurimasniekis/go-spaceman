import { Box } from '@mantine/core'
import { useEffect, useId, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { type AttachmentResolver, MARKDOWN_LIMITS, renderMarkdown } from '@/lib/markdown'
import { enhanceCode } from './markdown/enhance'
import { MermaidDiagram } from './markdown/MermaidDiagram'
import './composer/composer.css'
import './markdown/markdown.css'

type Diagram = { el: HTMLElement; source: string }

/**
 * Displays untrusted Markdown, sanitized, with the space's attachments resolved (SM-ATT-003, SM-UI-005): GFM, heading
 * anchors and footnotes, highlighted code blocks with a copy button, and Mermaid diagrams. The highlighter and mermaid
 * load lazily, only for bodies that need them.
 */
export function MarkdownView({ source, spaceId, resolveAttachment }: { source: string; spaceId: string; resolveAttachment?: AttachmentResolver }) {
  const id = useId()
  const html = useMemo(
    () => renderMarkdown(source, spaceId, { resolveAttachment, idPrefix: `user-content-${id.replace(/[^A-Za-z0-9]/g, '')}-` }),
    [source, spaceId, resolveAttachment, id],
  )
  const ref = useRef<HTMLDivElement>(null)
  const [diagrams, setDiagrams] = useState<Diagram[]>([])

  useEffect(() => {
    const root = ref.current
    if (!root) return
    const stop = enhanceCode(root)
    // Mermaid placeholders keep their source (shown until the diagram mounts, and when there are too many diagrams).
    const found = Array.from(root.querySelectorAll<HTMLElement>('[data-sm-mermaid]'))
      .slice(0, MARKDOWN_LIMITS.diagrams)
      .map((el) => {
        el.setAttribute('data-sm-mounted', '')
        return { el, source: el.querySelector('pre')?.textContent ?? '' }
      })
    setDiagrams((prev) => (prev.length || found.length ? found : prev))
    return stop
  }, [html])

  return (
    <>
      {/* The HTML is produced by marked and sanitized by DOMPurify (scripts, handlers, styles and remote images removed). */}
      <Box ref={ref} className="sm-markdown" fz="md" dangerouslySetInnerHTML={{ __html: html }} />
      {diagrams.map((d, i) => createPortal(<MermaidDiagram source={d.source} />, d.el, `${i}`))}
    </>
  )
}
