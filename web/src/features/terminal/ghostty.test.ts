import type { Terminal } from 'ghostty-web'
import { fitFont, scrollWithWheelAndTouch } from './ghostty'

vi.mock('ghostty-web', () => ({ init: vi.fn(), Terminal: class {} }))

/** A terminal whose canvas is `cols` whole-pixel cells wide, resized as soon as the font size is set (ghostty-web 0.4.0). */
function fakeTerm(fontSize: number, cols = 160) {
  const canvas = document.createElement('canvas')
  const element = document.createElement('div')
  element.appendChild(canvas)
  const sizes: number[] = []
  const options = {
    get fontSize() {
      return fontSize
    },
    set fontSize(v: number) {
      fontSize = v
      sizes.push(v)
    },
  }
  canvas.getBoundingClientRect = () => ({ width: Math.round(fontSize * 0.6) * cols }) as DOMRect
  return { term: { element, options } as unknown as Terminal, sizes, font: () => fontSize }
}

const boxOf = (width: number) => ({ clientWidth: width + 14 }) as HTMLElement

describe('fitFont', () => {
  it('settles on one size where whole-pixel cells made it alternate every frame', () => {
    // 160 columns in 892 px: 9 px fonts make 5 px cells (800 px), 10 px fonts 6 px cells (960 px, too wide). The old
    // proportional estimate went 9 → 10 → 9 … with every resize.
    const { term, font } = fakeTerm(9)
    const box = boxOf(892)
    fitFont(term, box)
    const settled = font()
    expect(Math.round(settled * 0.6) * 160).toBeLessThanOrEqual(892)
    for (let i = 0; i < 5; i++) {
      fitFont(term, box)
      expect(font()).toBe(settled)
    }
  })

  it('shrinks to fit and stops at the minimum', () => {
    const { term, font } = fakeTerm(13)
    fitFont(term, boxOf(1000))
    expect(Math.round(font() * 0.6) * 160).toBeLessThanOrEqual(1000)
    fitFont(term, boxOf(300))
    expect(font()).toBe(8)
  })
})

/** A terminal in a full-screen program's alternate screen that tracks the mouse with SGR reports (Claude Code's). */
function fullScreenTerm() {
  const element = document.createElement('div')
  const canvas = document.createElement('canvas')
  element.appendChild(canvas)
  canvas.getBoundingClientRect = () => ({ left: 0, top: 0, width: 1600, height: 960 }) as DOMRect // 10×20 px cells
  let wheel: ((e: WheelEvent) => boolean) | undefined
  const term = {
    element,
    cols: 160,
    rows: 48,
    viewportY: 0,
    buffer: { active: { type: 'alternate', cursorY: 47 } },
    getScrollbackLength: () => 0,
    scrollLines: vi.fn(),
    scrollToBottom: vi.fn(),
    getMode: (m: number) => m === 1006,
    hasMouseTracking: () => true,
    attachCustomWheelEventHandler: (h?: (e: WheelEvent) => boolean) => {
      wheel = h
    },
  }
  const box = document.createElement('div')
  box.appendChild(element)
  return { term: term as unknown as Terminal, box, wheel: (e: Partial<WheelEvent>) => wheel?.({ deltaMode: 0, clientX: 55, clientY: 45, ...e } as WheelEvent) }
}

/** A touch event the way the box's listeners read it (jsdom has no Touch constructor). */
function touch(type: string, y: number, x = 55) {
  const e = new Event(type, { cancelable: true })
  Object.defineProperty(e, 'touches', { value: type === 'touchend' ? [] : [{ clientX: x, clientY: y }] })
  return e
}

describe('scrollWithWheelAndTouch', () => {
  it('never writes to the input channel while watching, for the wheel or touch', () => {
    const { term, box, wheel } = fullScreenTerm()
    // The input channel: in watch mode the getter yields none, so only a takeover's handler could ever be written to.
    const holder = vi.fn()
    let holding = false
    const input = vi.fn(() => (holding ? holder : null))
    const stop = scrollWithWheelAndTouch(term, box, input)
    // Every wheel event is taken, so ghostty-web never types its own arrow keys; nothing is written.
    expect(wheel({ deltaY: -200 })).toBe(true)
    expect(wheel({ deltaY: 200 })).toBe(true)
    box.dispatchEvent(touch('touchstart', 100))
    box.dispatchEvent(touch('touchmove', 300))
    box.dispatchEvent(touch('touchmove', 100))
    box.dispatchEvent(touch('touchend', 0))
    expect(input).toHaveBeenCalled()
    expect(holder).not.toHaveBeenCalled()
    expect(term.scrollLines).not.toHaveBeenCalled()
    // The same view starts writing only once the viewer holds a takeover.
    holding = true
    wheel({ deltaY: -20 })
    expect(holder).toHaveBeenCalledWith('\x1b[<64;6;3M')
    stop()
  })

  it('sends SGR mouse-wheel reports at the pointer to a full-screen program during a takeover', () => {
    const { term, box, wheel } = fullScreenTerm()
    const sent: string[] = []
    const stop = scrollWithWheelAndTouch(term, box, () => (d) => sent.push(d))
    expect(wheel({ deltaY: -40 })).toBe(true) // two rows up, over the cell at column 6, row 3
    box.dispatchEvent(touch('touchstart', 100))
    const move = touch('touchmove', 80) // the finger moves up a row (row 5): scroll down
    box.dispatchEvent(move)
    expect(move.defaultPrevented).toBe(true)
    expect(sent).toEqual(['\x1b[<64;6;3M\x1b[<64;6;3M', '\x1b[<65;6;5M'])
    expect(sent.join('')).not.toContain('\x1b[A')
    stop()
  })
})
