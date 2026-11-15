/**
 * The Mermaid diagrams of a Markdown body, for the Mini App's "View diagram" page: the sources of its ```mermaid (or
 * ~~~mermaid) fences, in order. The body is untrusted: this only finds the text; MermaidDiagram renders it (strict
 * mode, sanitized, within planDiagrams' limits).
 */
import { MARKDOWN_LIMITS } from '@/lib/markdown'

const OPEN = /^ {0,3}(`{3,}|~{3,})[ \t]*mermaid\b/
const FENCE = /^ {0,3}(`{3,}|~{3,})[ \t]*$/

export function mermaidSources(body: string): string[] {
  const out: string[] = []
  const lines = body.split('\n')
  for (let i = 0; i < lines.length && out.length < MARKDOWN_LIMITS.diagrams; i++) {
    const open = OPEN.exec(lines[i])
    if (!open) continue
    const mark = open[1]
    const src: string[] = []
    for (i++; i < lines.length; i++) {
      const close = FENCE.exec(lines[i])
      if (close && close[1][0] === mark[0] && close[1].length >= mark.length) break
      src.push(lines[i])
    }
    out.push(src.join('\n'))
  }
  return out
}
