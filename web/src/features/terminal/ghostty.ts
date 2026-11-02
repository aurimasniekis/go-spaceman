import { init, Terminal } from 'ghostty-web'
import { terminalTheme } from '@/theme/nightowl'

let ready: Promise<void> | null = null

/** Loads the ghostty-web WebAssembly module once (the CSP allows 'wasm-unsafe-eval' for it, ADR-0020). */
export function loadGhostty(): Promise<void> {
  ready ??= init()
  return ready
}

/** Creates a watch-only terminal: keyboard input is never sent anywhere (SM-UI-004; takeover is a separate permission). */
export async function createTerminal(el: HTMLElement, cols = 120, rows = 40): Promise<Terminal> {
  await loadGhostty()
  const term = new Terminal({
    cols,
    rows,
    theme: terminalTheme,
    fontSize: 13,
    fontFamily: "'JetBrains Mono Variable', 'JetBrains Mono', ui-monospace, Menlo, monospace",
    disableStdin: true,
    scrollback: 10_000,
    cursorBlink: false,
  })
  term.open(el)
  return term
}

const MIN_FONT = 8
const MAX_FONT = 13

/**
 * Scales the font so the terminal's columns fit the container width (hc terminals default to 200 columns). Text below
 * MIN_FONT stays readable by scrolling horizontally instead.
 */
export function fitFont(term: Terminal, box: HTMLElement) {
  const canvas = term.element?.querySelector('canvas')
  if (!canvas) return
  const have = canvas.getBoundingClientRect().width
  const want = box.clientWidth - 14 // the box's padding and border
  if (have <= 0 || want <= 0) return
  const next = Math.max(MIN_FONT, Math.min(MAX_FONT, Math.floor((term.options.fontSize * want) / have * 2) / 2))
  if (next !== term.options.fontSize) term.options.fontSize = next
}
