import { Text, useComputedColorScheme } from '@mantine/core'
import { useEffect, useRef, useState } from 'react'
import { MARKDOWN_LIMITS } from '@/lib/markdown'
import './markdown.css'

let mermaidModule: Promise<typeof import('@/lib/mermaid')> | undefined
/** Loads lib/mermaid (and mermaid) once per page. */
const loadMermaid = () => (mermaidModule ??= import('@/lib/mermaid').catch((e: unknown) => {
    mermaidModule = undefined // a failed chunk load is retried next time
    throw e
  }))

/** True once the element has come near the viewport (always true without IntersectionObserver). */
function useNearViewport(ref: React.RefObject<HTMLElement | null>): boolean {
  const [near, setNear] = useState(typeof IntersectionObserver === 'undefined')
  useEffect(() => {
    if (near || !ref.current) return
    const io = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) setNear(true)
    }, { rootMargin: '300px' })
    io.observe(ref.current)
    return () => io.disconnect()
  }, [near, ref])
  return near
}

/** A short, single-line version of a render error (mermaid's messages can carry the whole parser state). */
function shortError(e: unknown): string {
  const msg = (e instanceof Error ? e.message : String(e)).split('\n').map((l) => l.trim()).filter(Boolean)
  return (msg.slice(0, 2).join(' ') || 'Unknown error').slice(0, 200)
}

type State = { svg?: string; error?: string }

/**
 * Renders one Mermaid diagram from its source (untrusted): mermaid loads lazily on first use, renders in strict mode
 * with the app's light or dark theme, and only when the diagram comes near the viewport. The source stays available as
 * the diagram's text alternative; a source that fails to render shows with a short error and never breaks the page.
 * Usable on its own (a "view diagram" page) as well as inside MarkdownView.
 */
export function MermaidDiagram({ source }: { source: string }) {
  const scheme = useComputedColorScheme('dark')
  const ref = useRef<HTMLElement>(null)
  const near = useNearViewport(ref)
  const [state, setState] = useState<State>({})
  useEffect(() => {
    if (!near) return
    if (source.length > MARKDOWN_LIMITS.diagramSource) {
      setState({ error: `The diagram is longer than ${MARKDOWN_LIMITS.diagramSource} characters, so it isn't rendered.` })
      return
    }
    let live = true
    loadMermaid()
      .then((m) => m.renderMermaid(source, scheme))
      .then(
        (svg) => live && setState({ svg }),
        (e: unknown) => live && setState({ error: shortError(e) }),
      )
    return () => {
      live = false
    }
  }, [source, scheme, near])

  return (
    <figure ref={ref} className="sm-mermaid-figure" data-state={state.svg ? 'rendered' : state.error ? 'error' : 'pending'}>
      {state.svg ? (
        // Sanitized SVG from lib/mermaid (strict mode, then DOMPurify).
        <div className="sm-mermaid-svg" role="img" aria-label="Diagram (its source follows)" dangerouslySetInnerHTML={{ __html: state.svg }} />
      ) : state.error ? (
        <Text size="sm" c="red" role="status">Couldn't render this diagram: {state.error}</Text>
      ) : (
        <Text size="sm" c="dimmed">Rendering diagram…</Text>
      )}
      <details className="sm-mermaid-source" open={!!state.error}>
        <summary>Diagram source</summary>
        <pre tabIndex={0}><code>{source}</code></pre>
      </details>
    </figure>
  )
}
