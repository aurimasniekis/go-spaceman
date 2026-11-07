/**
 * Scrolling a run terminal with the wheel or touch (pure logic; ghostty.ts wires it to ghostty-web 0.4.0).
 *
 * A terminal sits in a box that scrolls when the terminal is taller than it. Scrolling up moves the box first; at the
 * box's top it goes to the terminal (scrolling down retraces the same way: the scrollback, then the box):
 * - normal screen: the terminal's own scrollback moves, locally; nothing is sent to the program;
 * - alternate screen (a full-screen program keeps no scrollback): only a takeover's holder sends anything, and only
 *   what a terminal would: mouse-wheel reports while the program tracks the mouse, arrow keys under xterm's
 *   alternate-scroll mode (1007), else nothing. A watch-only viewer never sends input (SM-UI-004).
 *
 * ghostty-web itself turns every wheel event in an alternate screen into arrow keys, even when the program asked for
 * mouse reports: during a takeover Claude Code's full-screen renderer then answered "Scroll wheel is sending arrow keys
 * · use PgUp/PgDn to scroll" instead of scrolling.
 */

/** The parts of a ghostty-web Terminal scrolling uses. */
export interface ScrollTerm {
  readonly cols: number
  readonly rows: number
  /** Lines scrolled up from the bottom of the scrollback; 0 is live. */
  readonly viewportY: number
  getScrollbackLength(): number
  /** Positive scrolls toward the bottom (newer output). */
  scrollLines(amount: number): void
  scrollToBottom(): void
  getMode(mode: number, isAnsi?: boolean): boolean
  hasMouseTracking(): boolean
  readonly buffer: { readonly active: { readonly type: string; readonly cursorY: number } }
}

/** The scrolling box around the terminal. */
export interface ScrollBox {
  scrollTop: number
  readonly scrollHeight: number
  readonly clientHeight: number
}

/** A cell, 1-based, as mouse reports count them. */
export interface Cell {
  x: number
  y: number
}

const MODE_ALT_SCROLL = 1007
const MODE_SGR_MOUSE = 1006
/** At most this many reports or keys per gesture step, as ghostty-web does for its arrow keys. */
const MAX_STEPS = 5

/** Whether the program is in the alternate screen (no scrollback). */
export const inAlternateScreen = (term: ScrollTerm) => term.buffer.active.type === 'alternate'

/** What the program receives for a wheel step of `steps` lines (negative is up) in the alternate screen, or ''. */
export function wheelInput(term: ScrollTerm, steps: number, at: Cell): string {
  const n = Math.min(Math.abs(steps), MAX_STEPS)
  if (n === 0) return ''
  const up = steps < 0
  if (term.hasMouseTracking()) {
    const button = up ? 64 : 65
    const x = Math.max(1, Math.min(term.cols, at.x))
    const y = Math.max(1, Math.min(term.rows, at.y))
    const one = term.getMode(MODE_SGR_MOUSE)
      ? `\x1b[<${button};${x};${y}M`
      : // X10 encoding carries coordinates up to 223.
        `\x1b[M${String.fromCharCode(32 + button, 32 + Math.min(x, 223), 32 + Math.min(y, 223))}`
    return one.repeat(n)
  }
  if (term.getMode(MODE_ALT_SCROLL)) return (up ? '\x1b[A' : '\x1b[B').repeat(n)
  return ''
}

/** Moves the box by up to `px` pixels (negative is up); returns the pixels it could not take. */
export function scrollBoxBy(box: ScrollBox, px: number): number {
  const max = box.scrollHeight - box.clientHeight
  if (max <= 0 || px === 0) return px
  const before = box.scrollTop
  box.scrollTop = Math.max(0, Math.min(max, before + px))
  const rest = px - (box.scrollTop - before)
  return Math.abs(rest) < 1 ? 0 : rest // browsers round scrollTop
}

/** Whether the box itself can still move in a direction (negative is up). */
export function boxCanScroll(box: ScrollBox, direction: number): boolean {
  const max = box.scrollHeight - box.clientHeight
  if (max <= 0 || direction === 0) return false
  return direction < 0 ? box.scrollTop > 0 : box.scrollTop < max - 1
}

/**
 * Routes scroll gestures: the box first, then the scrollback or the program. `input` returns the takeover holder's
 * input, or null in watch mode. Pixel deltas are turned into lines with the row height; fractions carry over.
 */
export function createScroller(term: ScrollTerm, box: ScrollBox, rowHeight: () => number, input: () => ((data: string) => void) | null) {
  let carry = 0
  return {
    /** Scrolls by `px` pixels (negative is up) with the pointer over a cell. */
    by(px: number, at: Cell) {
      // The scrollback sits above the screen: going down, it returns to live output before the box moves.
      if (px > 0 && term.viewportY > 0 && !inAlternateScreen(term)) {
        const h = rowHeight()
        if (h <= 0) return
        carry += px / h
        const lines = Math.floor(carry)
        if (lines <= 0) return
        carry -= lines
        const take = Math.min(lines, Math.ceil(term.viewportY))
        term.scrollLines(take)
        if (lines > take) {
          carry = 0
          scrollBoxBy(box, (lines - take) * h)
        }
        return
      }
      const rest = scrollBoxBy(box, px)
      if (rest === 0) {
        carry = 0
        return
      }
      const h = rowHeight()
      if (h <= 0) return
      carry += rest / h
      const lines = carry < 0 ? Math.ceil(carry) : Math.floor(carry)
      if (lines === 0) return
      carry -= lines
      if (!inAlternateScreen(term)) {
        term.scrollLines(lines)
        return
      }
      const send = input()
      if (!send) return
      const data = wheelInput(term, lines, at)
      if (data) send(data)
    },
    reset() {
      carry = 0
    },
  }
}

/** A wheel event's vertical delta in pixels. */
export function wheelPixels(e: { deltaY: number; deltaMode: number }, rowHeight: number, pageHeight: number): number {
  if (e.deltaMode === 1) return e.deltaY * rowHeight // DOM_DELTA_LINE
  if (e.deltaMode === 2) return e.deltaY * pageHeight // DOM_DELTA_PAGE
  return e.deltaY
}

/** The box position that shows the cursor's row near the bottom (what following the output scrolls to). */
export function liveScrollTop(term: ScrollTerm, box: ScrollBox, rowHeight: number): number {
  const max = box.scrollHeight - box.clientHeight
  if (max <= 0 || rowHeight <= 0) return 0
  return Math.max(0, Math.min(max, (term.buffer.active.cursorY + 3) * rowHeight - box.clientHeight))
}

/** Whether the viewer has scrolled up, away from live output: into the scrollback, or the box above the cursor. */
export function scrolledAway(term: ScrollTerm, box: ScrollBox, rowHeight: number): boolean {
  if (term.viewportY > 0) return true
  if (rowHeight <= 0 || box.scrollHeight - box.clientHeight <= 0) return false
  return box.scrollTop < liveScrollTop(term, box, rowHeight) - rowHeight * 3
}

/** Back to live output: the bottom of the scrollback, and the box at the cursor. */
export function jumpToLive(term: ScrollTerm, box: ScrollBox, rowHeight: number) {
  term.scrollToBottom()
  box.scrollTop = liveScrollTop(term, box, rowHeight)
}

/**
 * Writes output without moving a viewer who scrolled into the scrollback: ghostty-web jumps to the bottom on every
 * write. The view stays on the same lines while new ones arrive below (until the scrollback's limit drops old lines).
 */
export function keepingPosition<T>(term: ScrollTerm, write: () => T): T {
  const y = Math.round(term.viewportY)
  const before = term.getScrollbackLength()
  const out = write()
  if (y > 0) {
    const grown = Math.max(0, term.getScrollbackLength() - before)
    const delta = Math.round(term.viewportY) - (y + grown)
    if (delta !== 0) term.scrollLines(delta)
  }
  return out
}
