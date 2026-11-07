import type { Terminal } from 'ghostty-web'
import { fitFont } from './ghostty'

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
