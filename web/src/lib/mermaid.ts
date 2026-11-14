/**
 * Mermaid diagrams from untrusted sources. Loaded lazily (only when a diagram is shown): mermaid is large.
 * Same mermaid version as the documentation site (site/package.json), which uses mermaid's defaults; here the
 * security level is pinned to 'strict' (labels are sanitized, no click handlers or links run scripts), the input size
 * and edge count are capped, and the SVG mermaid returns is sanitized once more before it reaches the DOM.
 */
import DOMPurify from 'dompurify'
import mermaid, { type MermaidConfig } from 'mermaid'
import { diagramStatements, MARKDOWN_LIMITS } from '@/lib/markdown'
import { lightOwl, nightOwl } from '@/theme/nightowl'

export type Scheme = 'light' | 'dark'

/** mermaid's default secure keys (11.17.2), kept when the list is replaced. */
const SECURE_DEFAULTS = ['secure', 'securityLevel', 'startOnLoad', 'maxTextSize', 'suppressErrorRendering', 'maxEdges']

const FONT = "'Inter Variable', Inter, system-ui, -apple-system, 'Segoe UI', sans-serif"

/** Mermaid's configuration for a color scheme: the 'base' theme with the app's Night Owl / Light Owl colors. */
export function mermaidConfig(scheme: Scheme): MermaidConfig {
  const p = scheme === 'dark' ? nightOwl : lightOwl
  return {
    startOnLoad: false,
    securityLevel: 'strict',
    suppressErrorRendering: true,
    maxTextSize: MARKDOWN_LIMITS.diagramSource,
    maxEdges: MARKDOWN_LIMITS.diagramEdges,
    // Keys a diagram's %%{init}%% directive or front matter can't change: mermaid's own, plus the theme (a diagram keeps
    // the app's colors and can't bring its own CSS).
    secure: [...SECURE_DEFAULTS, 'theme', 'themeVariables', 'themeCSS', 'darkMode', 'fontFamily', 'altFontFamily'],
    fontFamily: FONT,
    theme: 'base',
    darkMode: scheme === 'dark',
    themeVariables: {
      darkMode: scheme === 'dark',
      fontFamily: FONT,
      background: scheme === 'dark' ? p.bgRaised : '#ffffff',
      primaryColor: scheme === 'dark' ? p.border : p.bgRaised,
      primaryTextColor: p.fg,
      primaryBorderColor: p.blue,
      secondaryColor: scheme === 'dark' ? p.bg : p.selection,
      tertiaryColor: scheme === 'dark' ? p.bgRaised : '#ffffff',
      lineColor: p.teal,
      textColor: p.fg,
      noteBkgColor: scheme === 'dark' ? p.bg : '#fff8e1',
      noteTextColor: p.fg,
    },
  }
}

let seq = 0
let queue: Promise<unknown> = Promise.resolve()

/** Defense in depth over mermaid's own strict mode: an SVG (with HTML labels) and nothing that runs code. */
function sanitizeSVG(svg: string): string {
  return DOMPurify.sanitize(svg, {
    USE_PROFILES: { svg: true, svgFilters: true, html: true },
    // What mermaid's own sanitizer adds (its HTML labels and text alignment); tag names are compared lowercased.
    ADD_TAGS: ['foreignobject'],
    ADD_ATTR: ['dominant-baseline'],
    HTML_INTEGRATION_POINTS: { foreignobject: true },
    // <img> in an HTML label would load any URL (with the session cookie on the same origin); images in bodies render
    // only from attachments, and diagrams have none.
    FORBID_TAGS: ['script', 'iframe', 'object', 'embed', 'form', 'a', 'animate', 'set', 'image', 'img'],
    FORBID_ATTR: ['srcset', 'sizes'],
  })
}

async function render(source: string, scheme: Scheme): Promise<string> {
  if (source.length > MARKDOWN_LIMITS.diagramSource) throw new Error(`The diagram is longer than ${MARKDOWN_LIMITS.diagramSource} characters.`)
  if (diagramStatements(source) > MARKDOWN_LIMITS.diagramStatements) throw new Error(`The diagram has more than ${MARKDOWN_LIMITS.diagramStatements} statements.`)
  const id = `sm-mermaid-${++seq}`
  mermaid.initialize(mermaidConfig(scheme))
  try {
    const { svg } = await mermaid.render(id, source)
    return sanitizeSVG(svg)
  } finally {
    // mermaid renders in a temporary element and may leave it behind on errors.
    document.getElementById(id)?.remove()
    document.getElementById(`d${id}`)?.remove()
  }
}

/**
 * Renders one diagram to sanitized SVG markup. Renders run one at a time (mermaid's configuration is global, and the
 * theme is set per render). A render whose signal is aborted before its turn (its view is gone) is skipped and rejects
 * with an AbortError.
 */
export function renderMermaid(source: string, scheme: Scheme, signal?: AbortSignal): Promise<string> {
  const p = queue.then(() => {
    if (signal?.aborted) throw new DOMException('The diagram is no longer shown.', 'AbortError')
    return render(source, scheme)
  })
  queue = p.catch(() => undefined)
  return p
}
