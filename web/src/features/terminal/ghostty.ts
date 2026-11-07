import { init, Terminal } from 'ghostty-web'
import { terminalTheme } from '@/theme/nightowl'
import { boxCanScroll, createScroller, liveScrollTop, wheelPixels } from './scroll'

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
 *
 * Cell widths are whole pixels, so the width is not proportional to the font size: from 9 px (800 px wide, room to
 * spare) a proportional estimate picked 10 px (960 px, too wide), and from there 9 px again, a new size every frame
 * (the box's height changed with it, so its ResizeObserver kept calling). ghostty-web resizes the canvas as soon as
 * the size is set, so the estimate is measured and stepped down until it fits: the same width always ends at the same
 * size.
 */
export function fitFont(term: Terminal, box: HTMLElement) {
  const canvas = term.element?.querySelector('canvas')
  if (!canvas) return
  const width = () => canvas.getBoundingClientRect().width
  const have = width()
  const want = box.clientWidth - 14 // the box's padding and border
  if (have <= 0 || want <= 0) return
  const start = term.options.fontSize
  let next = Math.max(MIN_FONT, Math.min(MAX_FONT, Math.floor(((start * want) / have) * 2) / 2))
  if (next === start && have <= want) return
  term.options.fontSize = next
  while (next > MIN_FONT && width() > want) {
    next -= 0.5
    term.options.fontSize = next
  }
}

/** The height of one terminal row on screen, in CSS pixels (0 before the first render). */
export function rowHeight(term: Terminal): number {
  const canvas = term.element?.querySelector('canvas')
  if (!canvas || term.rows <= 0) return 0
  return canvas.getBoundingClientRect().height / term.rows
}

/**
 * Keeps the cursor's row in view as output arrives. hc terminals are 200×120 by default and a watch-only viewer never
 * resizes the program, so the screen is taller than the box; interactive programs such as Claude Code draw their
 * input near the cursor. Following stops while the viewer has scrolled elsewhere (the box, or into the scrollback) and
 * resumes once they scroll back near the cursor. Returns the cleanup.
 */
export function followCursor(term: Terminal, box: HTMLElement): () => void {
  let last = -1
  const follow = () => {
    const rowH = rowHeight(term)
    const max = box.scrollHeight - box.clientHeight
    if (rowH <= 0 || max <= 0 || term.viewportY > 0) return
    const want = liveScrollTop(term, box, rowH)
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

/** The cell under a pointer, 1-based. */
function cellAt(term: Terminal, clientX: number, clientY: number) {
  const r = term.element?.querySelector('canvas')?.getBoundingClientRect()
  if (!r || r.width <= 0 || r.height <= 0) return { x: 1, y: 1 }
  return { x: Math.floor(((clientX - r.left) / r.width) * term.cols) + 1, y: Math.floor(((clientY - r.top) / r.height) * term.rows) + 1 }
}

/**
 * Lets the mouse wheel and touch scroll the terminal (scroll.ts): the box around it first, then the terminal's
 * scrollback, or, in a full-screen program's alternate screen, the program itself, and only for a takeover's holder
 * (`input` returns null in watch mode). Every wheel event over the terminal is handled here, so ghostty-web never
 * turns it into arrow keys. Returns the cleanup.
 */
export function scrollWithWheelAndTouch(term: Terminal, box: HTMLElement, input: () => ((data: string) => void) | null): () => void {
  const scroller = createScroller(term, box, () => rowHeight(term), input)
  term.attachCustomWheelEventHandler((e) => {
    scroller.by(wheelPixels(e, rowHeight(term) || 16, box.clientHeight), cellAt(term, e.clientX, e.clientY))
    return true
  })
  // Touch: the browser scrolls the box natively; past its edge, a vertical drag scrolls the terminal instead.
  let last: { x: number; y: number } | null = null
  const start = (e: TouchEvent) => {
    last = e.touches.length === 1 ? { x: e.touches[0].clientX, y: e.touches[0].clientY } : null
    scroller.reset()
  }
  const move = (e: TouchEvent) => {
    if (!last || e.touches.length !== 1) return
    const t = e.touches[0]
    const dx = last.x - t.clientX
    const dy = last.y - t.clientY
    last = { x: t.clientX, y: t.clientY }
    if (Math.abs(dy) <= Math.abs(dx)) return
    // Left to the browser while the box can move, except down out of the scrollback (that comes first).
    if (boxCanScroll(box, dy) && !(dy > 0 && term.viewportY > 0)) return
    if (e.cancelable) e.preventDefault()
    scroller.by(dy, cellAt(term, t.clientX, t.clientY))
  }
  const end = () => {
    last = null
  }
  box.addEventListener('touchstart', start, { passive: true })
  box.addEventListener('touchmove', move, { passive: false })
  box.addEventListener('touchend', end)
  box.addEventListener('touchcancel', end)
  return () => {
    term.attachCustomWheelEventHandler(undefined)
    box.removeEventListener('touchstart', start)
    box.removeEventListener('touchmove', move)
    box.removeEventListener('touchend', end)
    box.removeEventListener('touchcancel', end)
  }
}
