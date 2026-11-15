import { CanvasRenderer, CellFlags, Ghostty, GhosttyTerminal, Terminal } from 'ghostty-web'
// The WebAssembly module of a later ghostty-web release than the JavaScript above (ADR-0076): 0.4.0's module left old
// cells in rows that scroll in at the bottom (upstream #180), which drew earlier output through Claude Code's lines.
import wasmUrl from 'ghostty-web-vt/ghostty-vt.wasm?url'
import { terminalTheme } from '@/theme/nightowl'
import { boxCanScroll, createScroller, liveScrollTop, wheelPixels } from './scroll'
import { SCROLLBACK_BYTES } from './scrollback'

const FONT_FAMILY = "'JetBrains Mono Variable', 'JetBrains Mono', ui-monospace, Menlo, monospace"
const FONT_SIZE = 13

/** The terminals open on the page (created and not yet disposed), each with the WebAssembly memory it runs in. */
const live = new Map<Terminal, WebAssembly.Memory | undefined>()

/** What the browser tests read through `window.__smTerminals` (the canvas has no text to query). */
export interface TerminalProbe {
  /** How many terminals are open. */
  count(): number
  /** Each open terminal's screen and scrollback as text, one string per terminal. */
  texts(): string[]
  /** The WebAssembly memory the open terminals hold, in bytes. */
  wasmBytes(): number
  /** Each open terminal's visible screen as text (cheap to read while output arrives, unlike texts). */
  screens(): string[]
}

/** A line of the terminal as text, column for column. */
interface ProbeLine {
  length: number
  getCell(x: number): { getChars(): string; getWidth(): number } | undefined
}

/**
 * A line's text with every column in place: an empty cell (one the program moved over and never wrote) reads as a
 * space, as in xterm.js. ghostty-web 0.4.0's translateToString leaves it out, which shifted the rest of the line left.
 */
function rowText(line: ProbeLine | undefined): string {
  if (!line) return ''
  let s = ''
  for (let x = 0; x < line.length; x++) {
    const cell = line.getCell(x)
    if (!cell || cell.getWidth() === 0) continue // the second half of a wide character
    s += cell.getChars() || ' '
  }
  return s.trimEnd()
}

/**
 * Installs the probe when a test asked for it before the page loaded (Playwright's addInitScript sets
 * `window.__smTerminalProbe = true`). Pages never install it otherwise.
 */
function installProbe() {
  if (typeof window === 'undefined') return
  const w = window as unknown as { __smTerminalProbe?: boolean; __smTerminals?: TerminalProbe }
  if (w.__smTerminalProbe !== true || w.__smTerminals) return
  w.__smTerminals = {
    count: () => live.size,
    texts: () =>
      [...live.keys()].map((t) => {
        const b = t.buffer.active
        const lines: string[] = []
        for (let y = 0; y < b.length; y++) lines.push(rowText(b.getLine(y)))
        return lines.join('\n')
      }),
    wasmBytes: () => [...live.values()].reduce((n, m) => n + (m?.buffer.byteLength ?? 0), 0),
    screens: () =>
      [...live.keys()].map((t) => {
        const b = t.buffer.active
        const lines: string[] = []
        for (let y = Math.max(0, b.length - t.rows); y < b.length; y++) lines.push(rowText(b.getLine(y)))
        return lines.join('\n')
      }),
  }
}

/** The ghostty-web WebAssembly module, compiled once per page: every terminal instantiates it (loadGhostty). */
let compiled: Promise<WebAssembly.Module> | null = null

/**
 * Compiles the module from its hashed asset (served cacheable, ADR-0076): compiling is asynchronous, off the main
 * thread, and happens once, not once per terminal. ghostty-web 0.4.0's own Ghostty.load() fetched and compiled the
 * module again from a 560 kB data: URL for every terminal. A failed compile is retried by the next terminal.
 */
export function wasmModule(): Promise<WebAssembly.Module> {
  compiled ??= (async () => {
    const res = await fetch(wasmUrl)
    if (!res.ok) throw new Error(`terminal module: HTTP ${res.status}`)
    // compileStreaming needs the application/wasm type; anything else (an odd proxy) compiles from the bytes.
    if (typeof WebAssembly.compileStreaming === 'function' && res.headers.get('Content-Type')?.startsWith('application/wasm')) {
      return WebAssembly.compileStreaming(res)
    }
    return WebAssembly.compile(await res.arrayBuffer())
  })().catch((e: unknown) => {
    compiled = null
    throw e
  })
  return compiled
}

/**
 * Loads a new instance of the ghostty-web WebAssembly module, one per terminal (the CSP allows 'wasm-unsafe-eval' for
 * it, ADR-0020).
 *
 * ghostty-web 0.4.0's init() shares one instance, and so one memory, between every terminal on the page, and a freed
 * terminal's memory is not cleared (wasm.test.ts, 2026-11-08):
 * - the next terminal starts out with the freed one's cells: after switching runs, lines of the previous run's
 *   terminal showed through the new one wherever it had not drawn yet, even with a full reset (RIS) written first;
 * - the memory of a terminal holding many styles (Claude Code's 24-bit colours) is never reused, so every such terminal
 *   viewed grows the page's memory for good (about 50 MB for a full scrollback) until the tab freezes and crashes;
 * - without a full reset first, the third terminal's write failed with "memory access out of bounds" (the earlier
 *   hang after closing a full-screen program, which the reset worked around).
 * A terminal of its own instance starts from zeroed memory (the module defines its memory; nothing is imported but a
 * log function), and its whole memory goes with it. Only the compiled code is shared (wasmModule), which holds no
 * state.
 */
export async function loadGhostty(): Promise<Ghostty> {
  const module = await wasmModule()
  let memory: WebAssembly.Memory | undefined
  // The import ghostty-web's own loader gives the module: its log messages, read from this instance's memory.
  const log = (ptr: number, len: number) => {
    if (memory) console.log('[ghostty-vt]', new TextDecoder().decode(new Uint8Array(memory.buffer, ptr, len)))
  }
  const instance = await WebAssembly.instantiate(module, { env: { log } })
  memory = instance.exports.memory as WebAssembly.Memory
  patchGhostty()
  return new Ghostty(instance)
}

/** The ghostty-web version the prototype patches below were written against and verified with (ADR-0076). */
export const PATCHED_GHOSTTY_WEB = '0.4.0'

let warned = false

/**
 * Applies the prototype patches (cacheViewport, skipBlankCells) when the installed ghostty-web is the version they were
 * written against. They replace private methods of 0.4.0 whose behaviour they depend on, so on any other version they
 * are left out (drawing is slower, never wrong) with a console warning: re-verify them before allowing that version.
 * Returns whether they are applied.
 */
export function patchGhostty(
  version: string = __GHOSTTY_WEB_VERSION__,
  protos: { terminal?: object; renderer?: object } = {},
): boolean {
  if (version !== PATCHED_GHOSTTY_WEB) {
    if (!warned) console.warn(`ghostty-web ${version}: terminal drawing patches skipped (written for ${PATCHED_GHOSTTY_WEB}, ADR-0076)`)
    warned = true
    return false
  }
  cacheViewport(protos.terminal)
  skipBlankCells(protos.renderer)
  return true
}

const patched = Symbol.for('spaceman.ghostty.viewportCache')

/**
 * Makes drawing a terminal linear in its size again. ghostty-web 0.4.0's GhosttyTerminal.getLine(y) copies and parses
 * the whole viewport (every cell of every row) to return one row, and its renderer calls it for every row it draws, so
 * a full redraw parsed rows × rows × columns cells (2.9 million for a 200×120 terminal), on every frame of output that
 * scrolls (ADR-0076: it was most of a 22 MB recording's 33 s of blocked page at 4× CPU throttling, with the catch-up
 * applied record by record). Here the parsed viewport is kept until the terminal changes
 * (write, resize, free) or the current task ends, whichever comes first, so one redraw parses the viewport once.
 * getViewport is the only reader of the parsed cells and the only writer of the pool it returns (0.4.0 source).
 */
export function cacheViewport(proto: object = GhosttyTerminal.prototype) {
  const p = proto as Record<string | symbol, unknown> & { [k: string]: unknown }
  if (p[patched] || typeof p.getViewport !== 'function') return
  p[patched] = true
  const views = new WeakMap<object, unknown>()
  const getViewport = p.getViewport as (this: object) => unknown
  p.getViewport = function (this: object) {
    if (views.has(this)) return views.get(this)
    const cells = getViewport.call(this)
    views.set(this, cells)
    queueMicrotask(() => views.delete(this))
    return cells
  }
  for (const name of ['write', 'resize', 'free']) {
    const orig = p[name] as ((this: object, ...args: unknown[]) => unknown) | undefined
    if (typeof orig !== 'function') continue
    p[name] = function (this: object, ...args: unknown[]) {
      views.delete(this)
      return orig.apply(this, args)
    }
  }
}
/**
 * Waits (briefly) for the terminal font, so the first terminal of a page measures its cells in the font it draws with:
 * ghostty-web 0.4.0 measures the font when the terminal opens and when its size or family changes, never when a web
 * font finishes loading, so a terminal opened before then keeps the fallback font's cell size.
 */
async function fontReady(): Promise<void> {
  if (typeof document === 'undefined' || !document.fonts?.load) return
  await Promise.race([document.fonts.load(`${FONT_SIZE}px ${FONT_FAMILY}`).catch(() => []), new Promise((r) => setTimeout(r, 1_500))])
}

/**
 * Creates a watch-only terminal: keyboard input is never sent anywhere (SM-UI-004). Only a takeover's holder turns
 * input on (TerminalView), and the server accepts their keys only while they hold it. Every terminal runs in its own
 * WebAssembly instance (loadGhostty); dispose it with disposeTerminal.
 */
export async function createTerminal(el: HTMLElement, cols = 120, rows = 40): Promise<Terminal> {
  const [ghostty] = await Promise.all([loadGhostty(), fontReady()])
  const term = new Terminal({
    ghostty,
    cols,
    rows,
    theme: terminalTheme,
    fontSize: FONT_SIZE,
    fontFamily: FONT_FAMILY,
    disableStdin: true,
    scrollback: SCROLLBACK_BYTES, // bytes, not lines (scrollback.ts)
    cursorBlink: false,
  })
  term.open(el)
  installProbe()
  live.set(term, (ghostty as unknown as { memory?: WebAssembly.Memory }).memory)
  return term
}

/**
 * Disposes a terminal: its render loop, listeners, canvas and WebAssembly state. Nothing else holds its instance, so
 * the instance's memory is released with it.
 */
export function disposeTerminal(term: Terminal) {
  live.delete(term)
  term.dispose()
}

/** A cell as ghostty-web 0.4.0's renderer reads it. */
interface Cell {
  codepoint: number
  flags: number
  grapheme_len: number
  hyperlink_id: number
}

const skipping = Symbol.for('spaceman.ghostty.skipBlankCells')

/**
 * Leaves out the text of blank cells when drawing. ghostty-web 0.4.0's renderer sets the font and calls fillText for
 * every cell of a changed row, a space included, and most of a terminal is blank, so a screen of output that scrolled
 * cost a canvas call per cell on every frame. A blank cell has no ink (its background is drawn separately,
 * renderCellBackground), so it is skipped unless it carries a line of its own: underline, strikethrough, or a hovered
 * link.
 */
export function skipBlankCells(proto: object = CanvasRenderer.prototype) {
  const p = proto as Record<string | symbol, unknown>
  if (p[skipping] || typeof p.renderCellText !== 'function') return
  p[skipping] = true
  const lines = CellFlags.UNDERLINE | CellFlags.STRIKETHROUGH
  const renderCellText = p.renderCellText as (this: object, cell: Cell, ...rest: unknown[]) => unknown
  p.renderCellText = function (this: { hoveredHyperlinkId?: number; hoveredLinkRange?: unknown }, cell: Cell, ...rest: unknown[]) {
    const blank = (cell.codepoint === 0 || cell.codepoint === 32) && cell.grapheme_len === 0 && (cell.flags & lines) === 0
    if (blank && !this.hoveredLinkRange && !(cell.hyperlink_id > 0 && cell.hyperlink_id === this.hoveredHyperlinkId)) return
    return renderCellText.call(this, cell, ...rest)
  }
}

/** The parts of a ghostty-web 0.4.0 Terminal that holdDrawing reaches (private in its typings). */
interface Drawn {
  renderer?: { render(buffer: unknown, forceAll: boolean, viewportY: number, terminal: unknown, scrollbarOpacity?: number): void }
  wasmTerm?: unknown
  viewportY: number
  scrollbarOpacity?: number
}

/**
 * Stops a terminal drawing until the returned function is called, which draws it once in full. ghostty-web 0.4.0
 * redraws every changed row on every animation frame, and output that scrolls changes every row: while a backlog of
 * stored output was applied a slice at a time, the frames in between redrew the whole screen (a canvas fillText per
 * cell) six times longer than the output took to parse. A terminal catching up shows "Loading output…" instead and
 * draws when it has caught up.
 */
export function holdDrawing(term: Terminal): () => void {
  const t = term as unknown as Drawn
  const renderer = t.renderer
  if (!renderer || typeof renderer.render !== 'function') return () => {}
  const own = Object.prototype.hasOwnProperty.call(renderer, 'render')
  const render = renderer.render
  renderer.render = () => {}
  let held = true
  return () => {
    if (!held) return
    held = false
    if (own) renderer.render = render
    else delete (renderer as { render?: unknown }).render
    if (t.renderer === renderer && t.wasmTerm) renderer.render(t.wasmTerm, true, t.viewportY, term, t.scrollbarOpacity ?? 0)
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
  // Every size set redraws the whole terminal (ghostty-web 0.4.0): draw once, at the size that fits.
  const release = holdDrawing(term)
  try {
    term.options.fontSize = next
    while (next > MIN_FONT && width() > want) {
      next -= 0.5
      term.options.fontSize = next
    }
  } finally {
    release()
  }
}

/**
 * The character cell at the regular font size, in CSS pixels: "Fit to window" sizes the run's terminal so its text is
 * shown at the regular size (ADR-0075). It is measured the way ghostty-web 0.4.0's renderer measures its cell
 * (measureFont: the width of "M" rounded up, the ink height rounded up plus 2), at MAX_FONT whatever size fitFont
 * shows now. Scaling the canvas's current cell instead was unstable: cell widths are whole pixels at every font size,
 * so the same window fitted to 90×37 or 91×41 depending on the size shown at the time.
 */
export function regularCell(term: Terminal): { width: number; height: number } | null {
  if (typeof document === 'undefined') return null
  const ctx = document.createElement('canvas').getContext('2d')
  if (!ctx) return null
  ctx.font = `${MAX_FONT}px ${term.options.fontFamily || FONT_FAMILY}`
  const m = ctx.measureText('M')
  const ascent = m.actualBoundingBoxAscent || MAX_FONT * 0.8
  const descent = m.actualBoundingBoxDescent || MAX_FONT * 0.2
  const width = Math.ceil(m.width)
  return width > 0 ? { width, height: Math.ceil(ascent + descent) + 2 } : null
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
