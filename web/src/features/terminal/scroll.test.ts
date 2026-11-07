import { boxCanScroll, createScroller, jumpToLive, keepingPosition, scrollBoxBy, scrolledAway, wheelInput, wheelPixels, type ScrollTerm } from './scroll'

/** A terminal with a scrollback of `scrollback` lines, the way ghostty-web 0.4.0 moves its viewport. */
class FakeTerm implements ScrollTerm {
  cols = 160
  rows = 48
  viewportY = 0
  scrollback = 0
  modes = new Set<number>()
  mouse = false
  buffer = { active: { type: 'normal', cursorY: 47 } }
  getScrollbackLength() {
    return this.scrollback
  }
  scrollLines(n: number) {
    this.viewportY = Math.max(0, Math.min(this.scrollback, this.viewportY - n))
  }
  scrollToBottom() {
    this.viewportY = 0
  }
  getMode(m: number) {
    return this.modes.has(m)
  }
  hasMouseTracking() {
    return this.mouse
  }
  /** Output that adds lines to the scrollback; like ghostty-web, a write jumps to the bottom. */
  write(lines: number) {
    this.scrollback += lines
    this.viewportY = 0
  }
}

const box = (scrollTop = 0, scrollHeight = 800, clientHeight = 400) => ({ scrollTop, scrollHeight, clientHeight })
const ROW = 16
const at = { x: 10, y: 5 }

describe('terminal scrolling', () => {
  it('moves the box first, then the scrollback, without sending input', () => {
    const term = new FakeTerm()
    term.scrollback = 500
    const b = box(100)
    const s = createScroller(term, b, () => ROW, () => null)
    s.by(-80, at)
    expect(b.scrollTop).toBe(20)
    expect(term.viewportY).toBe(0)
    // The rest of a gesture past the box's top scrolls the scrollback: 20 px moves the box, 160 px are 10 lines.
    s.by(-180, at)
    expect(b.scrollTop).toBe(0)
    expect(term.viewportY).toBe(10)
    s.by(-1600, at)
    expect(term.viewportY).toBe(110)
    // Back down: the scrollback first (the box is at its top, the gesture comes back through the scrollback).
    s.by(1600, at)
    expect(term.viewportY).toBe(10)
    expect(b.scrollTop).toBe(0)
    // 25 lines: the last 10 of the scrollback, then 15 rows of the box.
    s.by(400, at)
    expect(term.viewportY).toBe(0)
    expect(b.scrollTop).toBe(240)
  })

  it('carries fractions of a line between small wheel steps', () => {
    const term = new FakeTerm()
    term.scrollback = 100
    const s = createScroller(term, box(0, 400, 400), () => ROW, () => null)
    for (let i = 0; i < 4; i++) s.by(-4, at)
    expect(term.viewportY).toBe(1)
  })

  it('never sends input in watch mode, even in a full-screen program', () => {
    const term = new FakeTerm()
    term.buffer.active.type = 'alternate'
    term.mouse = true
    term.modes.add(1006).add(1007)
    const sent: string[] = []
    let holder: ((d: string) => void) | null = null
    const s = createScroller(term, box(0, 400, 400), () => ROW, () => holder)
    s.by(-320, at)
    s.by(320, at)
    expect(term.viewportY).toBe(0)
    // The same scroller sends once the viewer holds a takeover, and stops again when it ends.
    holder = (d) => sent.push(d)
    s.by(-16, at)
    holder = null
    s.by(-16, at)
    expect(sent).toEqual(['\x1b[<64;10;5M'])
  })

  it('sends mouse-wheel reports to a full-screen program that tracks the mouse during a takeover', () => {
    const term = new FakeTerm()
    term.buffer.active.type = 'alternate'
    term.mouse = true
    term.modes.add(1006)
    const sent: string[] = []
    const s = createScroller(term, box(0, 400, 400), () => ROW, () => (d) => sent.push(d))
    s.by(-32, at)
    s.by(16, { x: 999, y: 0 })
    // Two lines up at the pointer's cell, one down clamped into the screen; never arrow keys (Claude Code's
    // full-screen renderer answered arrows with "Scroll wheel is sending arrow keys").
    expect(sent).toEqual(['\x1b[<64;10;5M\x1b[<64;10;5M', '\x1b[<65;160;1M'])
    expect(sent.join('')).not.toContain('\x1b[A')
  })

  it('encodes reports without SGR mode, and arrows only under alternate scroll mode', () => {
    const term = new FakeTerm()
    term.buffer.active.type = 'alternate'
    term.mouse = true
    expect(wheelInput(term, -1, { x: 1, y: 1 })).toBe('\x1b[M`!!')
    expect(wheelInput(term, 9, { x: 1, y: 1 })).toBe('\x1b[Ma!!'.repeat(5))
    term.mouse = false
    expect(wheelInput(term, -2, at)).toBe('')
    term.modes.add(1007)
    expect(wheelInput(term, -2, at)).toBe('\x1b[A\x1b[A')
    expect(wheelInput(term, 1, at)).toBe('\x1b[B')
    expect(wheelInput(term, 0, at)).toBe('')
  })

  it('keeps a scrolled-up viewer on the same lines while output arrives', () => {
    const term = new FakeTerm()
    term.scrollback = 300
    term.scrollLines(-40)
    keepingPosition(term, () => term.write(5))
    expect(term.viewportY).toBe(45)
    // At live output, new output stays live.
    term.scrollToBottom()
    keepingPosition(term, () => term.write(5))
    expect(term.viewportY).toBe(0)
  })

  it('tells when the viewer scrolled away and jumps back to live output', () => {
    const term = new FakeTerm()
    term.scrollback = 300
    const b = box(400, 800, 400) // the cursor's row (47) is near the bottom: live is the box's end
    expect(scrolledAway(term, b, ROW)).toBe(false)
    b.scrollTop = 100
    expect(scrolledAway(term, b, ROW)).toBe(true)
    b.scrollTop = 400
    term.scrollLines(-10)
    expect(scrolledAway(term, b, ROW)).toBe(true)
    b.scrollTop = 0
    jumpToLive(term, b, ROW)
    expect(term.viewportY).toBe(0)
    expect(b.scrollTop).toBe(400)
    expect(scrolledAway(term, b, ROW)).toBe(false)
    // A terminal that fits its box with nothing in the scrollback is never away.
    expect(scrolledAway(new FakeTerm(), box(0, 400, 400), ROW)).toBe(false)
  })

  it('measures wheel deltas and box edges', () => {
    expect(wheelPixels({ deltaY: 3, deltaMode: 1 }, ROW, 400)).toBe(48)
    expect(wheelPixels({ deltaY: -1, deltaMode: 2 }, ROW, 400)).toBe(-400)
    expect(wheelPixels({ deltaY: 7, deltaMode: 0 }, ROW, 400)).toBe(7)
    const b = box(0)
    expect(boxCanScroll(b, -1)).toBe(false)
    expect(boxCanScroll(b, 1)).toBe(true)
    expect(scrollBoxBy(b, 1000)).toBe(600)
    expect(boxCanScroll(b, 1)).toBe(false)
    expect(scrollBoxBy(box(0, 400, 400), -5)).toBe(-5)
  })
})
