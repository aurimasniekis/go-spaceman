/**
 * Code syntax highlighting with @tanstack/highlight: a small, synchronous, class-based highlighter (tokens get
 * `th-token th-<kind>` classes, colored by the app's light and dark tokens in components/markdown/markdown.css).
 * This module is loaded lazily (only when a body has a fenced block with a language) and loads each grammar lazily too.
 */
import { createHighlighter, type HighlightToken, type Highlighter, type LanguageDefinition } from '@tanstack/highlight/core'
import { ALIASES, LANGUAGES } from './highlight-languages'

const loaded = new Map<string, LanguageDefinition>()
let highlighter: Highlighter | undefined

/** The grammar name for a fence language, or undefined when there is none (plain text). */
export function languageName(lang: string): string | undefined {
  const name = Object.hasOwn(ALIASES, lang) ? ALIASES[lang] : lang
  return name !== 'plaintext' && Object.hasOwn(LANGUAGES, name) ? name : undefined
}

/** The tokens of code in a fence language; undefined for unknown languages. */
export async function tokenize(code: string, lang: string): Promise<HighlightToken[] | undefined> {
  const name = languageName(lang)
  if (!name) return undefined
  if (!loaded.has(name)) {
    loaded.set(name, await LANGUAGES[name]())
    highlighter = undefined
  }
  highlighter ??= createHighlighter({ languages: [...loaded.values()] })
  return highlighter.tokenize(code, { lang: name }).tokens
}

/** Replaces an element's content with the tokens as spans. Built from text nodes, so no HTML is ever parsed. */
export function paint(el: HTMLElement, tokens: HighlightToken[]) {
  const frag = el.ownerDocument.createDocumentFragment()
  for (const t of tokens) {
    if (!t.className) {
      frag.append(t.value)
      continue
    }
    const span = el.ownerDocument.createElement('span')
    span.className = `th-token th-${t.className}`
    span.textContent = t.value
    frag.append(span)
  }
  el.replaceChildren(frag)
}
