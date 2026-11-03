import { init, Terminal } from 'ghostty-web'
import { terminalTheme } from '@/theme/nightowl'

let ready: Promise<void> | null = null

/** Loads the ghostty-web WebAssembly module once (the CSP allows 'wasm-unsafe-eval' for it, ADR-0020). */
export function loadGhostty(): Promise<void> {
  ready ??= init()
  return ready
}

/**
 * Creates a watch-only terminal: keyboard input is never sent anywhere (SM-UI-004). Only a takeover's holder turns
 * input on (TerminalView), and the server accepts their keys only while they hold it.
 */
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

/**
 * Disposes a terminal so the next one on the page still works. With ghostty-web 0.4.0, disposing a terminal that drew a
 * full-screen program (Claude Code's alternate screen at 200×120) leaves the shared WebAssembly module in a state where
 * the next terminal's resize never returns and the tab hangs (Chrome: RESULT_CODE_HUNG); a first page load is fine.
 * A full reset (RIS, ESC c) written first avoids it; Terminal.reset() does not. Reproduced with a real Claude Code
 * recording, 2026-11-03.
 */
export function disposeTerminal(term: Terminal) {
  try {
    term.write('\x1bc')
  } finally {
    term.dispose()
  }
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

/**
 * Keeps the cursor's row in view as output arrives. hc terminals are 200×120 by default and a watch-only viewer never
 * resizes the program, so the screen is taller than the box; interactive programs such as Claude Code draw their
 * input near the cursor. Following stops while the viewer has scrolled elsewhere and resumes once they scroll back
 * near the cursor. Returns the cleanup.
 */
export function followCursor(term: Terminal, box: HTMLElement): () => void {
  let last = -1
  const follow = () => {
    const canvas = term.element?.querySelector('canvas')
    if (!canvas || term.rows <= 0) return
    const rowH = canvas.getBoundingClientRect().height / term.rows
    const max = box.scrollHeight - box.clientHeight
    if (rowH <= 0 || max <= 0) return
    const want = Math.max(0, Math.min(max, (term.buffer.active.cursorY + 3) * rowH - box.clientHeight))
    const away = last >= 0 && Math.abs(box.scrollTop - last) > 2 && Math.abs(box.scrollTop - want) > rowH * 3
    if (away) return
    box.scrollTop = want
    last = box.scrollTop
  }
  // ghostty-web 0.4.0 never fires onRender; its render loop fires onCursorMove when the cursor changes rows.
  const moved = term.onCursorMove(() => requestAnimationFrame(follow))
  const resized = term.onResize(() => requestAnimationFrame(follow))
  const ro = new ResizeObserver(() => requestAnimationFrame(follow))
  ro.observe(box)
  return () => {
    moved.dispose()
    resized.dispose()
    ro.disconnect()
  }
}

/**
 * Lets the mouse wheel scroll the box around the terminal. ghostty-web cancels every wheel event over the terminal (in
 * an alternate screen it turns them into arrow keys for the program), so a terminal taller than its box could not be
 * scrolled with the wheel. The box scrolls first; at its top or bottom the wheel goes to ghostty-web (scrollback).
 * Returns the cleanup.
 */
export function wheelScrollsBox(term: Terminal, box: HTMLElement): () => void {
  term.attachCustomWheelEventHandler((e) => {
    const max = box.scrollHeight - box.clientHeight
    if (max <= 0) return false
    const dy = e.deltaMode === WheelEvent.DOM_DELTA_LINE ? e.deltaY * 16 : e.deltaMode === WheelEvent.DOM_DELTA_PAGE ? e.deltaY * box.clientHeight : e.deltaY
    if ((dy > 0 && box.scrollTop < max - 1) || (dy < 0 && box.scrollTop > 0)) {
      box.scrollTop = Math.max(0, Math.min(max, box.scrollTop + dy))
      return true
    }
    return false
  })
  return () => term.attachCustomWheelEventHandler(undefined)
}
