import type { Terminal } from 'ghostty-web'
import { createTerminal, disposeTerminal, fitFont, holdDrawing, scrollWithWheelAndTouch, skipBlankCells } from './ghostty'

const ghostty = vi.hoisted(() => {
  const created: { options: { ghostty?: unknown }; disposed: boolean }[] = []
  return {
    created,
    Ghostty: class {
      constructor(public instance: unknown) {}
    },
    GhosttyTerminal: class {},
    CanvasRenderer: class {},
    Terminal: class {
      disposed = false
      constructor(public options: { ghostty?: unknown }) {
        created.push(this)
      }
      open() {}
      dispose() {
        this.disposed = true
      }
    },
  }
})
vi.mock('ghostty-web', () => ({
  Ghostty: ghostty.Ghostty,
  GhosttyTerminal: ghostty.GhosttyTerminal,
  CanvasRenderer: ghostty.CanvasRenderer,
  CellFlags: { UNDERLINE: 4, STRIKETHROUGH: 8 },
  Terminal: ghostty.Terminal,
}))

describe('createTerminal', () => {
  it('compiles the module once and runs every terminal in an instance of its own, released on dispose', async () => {
    const fetch = vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(new Uint8Array([0, 97, 115, 109]), { headers: { 'Content-Type': 'application/octet-stream' } }))
    const module = {} as WebAssembly.Module
    const compile = vi.spyOn(WebAssembly, 'compile').mockResolvedValue(module)
    let n = 0
    const instantiate = vi.spyOn(WebAssembly, 'instantiate').mockImplementation((async () => ({ exports: { memory: { n: ++n } } })) as never)
    try {
      const a = await createTerminal(document.createElement('div'))
      const b = await createTerminal(document.createElement('div'))
      expect(fetch).toHaveBeenCalledTimes(1)
      expect(compile).toHaveBeenCalledTimes(1)
      expect(instantiate).toHaveBeenCalledTimes(2)
      expect(instantiate.mock.calls.every(([m]) => m === module)).toBe(true)
      const [ta, tb] = ghostty.created
      expect((ta.options.ghostty as { instance: unknown }).instance).toEqual({ exports: { memory: { n: 1 } } })
      expect((tb.options.ghostty as { instance: unknown }).instance).toEqual({ exports: { memory: { n: 2 } } })
      disposeTerminal(a)
      expect(ta.disposed).toBe(true)
      expect(tb.disposed).toBe(false)
      disposeTerminal(b)
    } finally {
      fetch.mockRestore()
      compile.mockRestore()
      instantiate.mockRestore()
    }
  })
})

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

describe('holdDrawing', () => {
  class Renderer {
    calls: unknown[][] = []
    render(...args: unknown[]) {
      this.calls.push(args)
    }
  }
  const termWith = (renderer: Renderer) => ({ renderer, wasmTerm: { id: 'wasm' }, viewportY: 3, scrollbarOpacity: 0 }) as unknown as Terminal

  it('draws nothing while held and the whole terminal once on release', () => {
    const r = new Renderer()
    const term = termWith(r)
    const release = holdDrawing(term)
    r.render({}, false, 0)
    expect(r.calls).toHaveLength(0)
    release()
    expect(r.calls).toEqual([[{ id: 'wasm' }, true, 3, term, 0]])
    release() // only once
    expect(r.calls).toHaveLength(1)
    r.render('frame')
    expect(r.calls).toHaveLength(2) // drawing again
    expect(Object.prototype.hasOwnProperty.call(r, 'render')).toBe(false)
  })

  it('nests: an inner hold (fitFont during a catch-up) leaves the outer one in place', () => {
    const r = new Renderer()
    const term = termWith(r)
    const outer = holdDrawing(term)
    const inner = holdDrawing(term)
    inner()
    r.render('frame')
    expect(r.calls).toHaveLength(0)
    outer()
    expect(r.calls).toHaveLength(1)
  })
})

describe('skipBlankCells', () => {
  it('skips the text of blank cells only', () => {
    const drawn: number[] = []
    class Renderer {
      hoveredLinkRange: unknown = null
      hoveredHyperlinkId = 0
      renderCellText(cell: { codepoint: number }) {
        drawn.push(cell.codepoint)
      }
    }
    skipBlankCells(Renderer.prototype)
    skipBlankCells(Renderer.prototype) // once
    const r = new Renderer()
    const cell = (codepoint: number, flags = 0, more: object = {}) => ({ codepoint, flags, grapheme_len: 0, hyperlink_id: 0, ...more })
    r.renderCellText(cell(0))
    r.renderCellText(cell(32))
    r.renderCellText(cell(65))
    r.renderCellText(cell(32, 4)) // underlined
    r.renderCellText(cell(32, 8)) // struck through
    r.renderCellText(cell(32, 0, { grapheme_len: 2 }))
    r.renderCellText(cell(0, 0, { hyperlink_id: 7 })) // a link, not hovered
    r.hoveredHyperlinkId = 7
    r.renderCellText(cell(0, 0, { hyperlink_id: 7 })) // the hovered link
    r.hoveredLinkRange = { startY: 0 }
    r.renderCellText(cell(0))
    expect(drawn).toEqual([65, 32, 32, 32, 0, 0])
  })
})
