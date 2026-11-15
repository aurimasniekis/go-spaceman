// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { Ghostty } from 'ghostty-web'

/**
 * The terminal runs ghostty-web 0.4.0's JavaScript on the WebAssembly module of a later release (ghostty-web-vt,
 * ADR-0076). The pairing holds only while the module offers everything that JavaScript calls, imports nothing more
 * than the log function loadGhostty gives it, and reads the terminal configuration in the layout 0.4.0 writes.
 */
const require = createRequire(import.meta.url)
const shipped = readFileSync(require.resolve('ghostty-web-vt/ghostty-vt.wasm'))
const js = readFileSync(require.resolve('ghostty-web'), 'utf8')

describe('the WebAssembly module under ghostty-web 0.4.0’s JavaScript', () => {
  const module = new WebAssembly.Module(shipped)

  it('exports every function the JavaScript calls', () => {
    const called = new Set([...js.matchAll(/\bexports\.(ghostty_\w+)/g)].map((m) => m[1]))
    expect(called.size).toBeGreaterThan(40)
    const exported = new Set(WebAssembly.Module.exports(module).map((e) => e.name))
    expect([...called].filter((name) => !exported.has(name))).toEqual([])
  })

  it('imports only the log function', () => {
    expect(WebAssembly.Module.imports(module)).toEqual([{ module: 'env', name: 'log', kind: 'function' }])
  })

  it('reads the configuration 0.4.0 writes (scrollback, colours, palette)', async () => {
    const g = new Ghostty((await WebAssembly.instantiate(shipped, { env: { log: () => {} } })).instance)
    const palette = Array.from({ length: 16 }, (_, i) => 0x100000 * (i + 1) + 0x0203 * i)
    const t = g.createTerminal(40, 5, { scrollbackLimit: 100_000, fgColor: 0x123456, bgColor: 0x0a0b0c, cursorColor: 0xffffff, palette })
    t.update() // the colours are read from the render state
    const colors = t.getColors()
    expect(colors.foreground).toEqual({ r: 0x12, g: 0x34, b: 0x56 })
    expect(colors.background).toEqual({ r: 0x0a, g: 0x0b, b: 0x0c })
    t.write('\x1b[31mR\x1b[94mB\x1b[0mD')
    const [r, b, d] = t.getLine(0)!
    const rgb = (n: number) => ({ r: (n >> 16) & 255, g: (n >> 8) & 255, b: n & 255 })
    expect({ r: r.fg_r, g: r.fg_g, b: r.fg_b }).toEqual(rgb(palette[1]))
    expect({ r: b.fg_r, g: b.fg_g, b: b.fg_b }).toEqual(rgb(palette[12]))
    expect({ r: d.fg_r, g: d.fg_g, b: d.fg_b }).toEqual(rgb(0x123456))
    // The scrollback limit (bytes) is read too: a larger one keeps more lines.
    const lines = Array.from({ length: 4000 }, (_, i) => `${i} ${'y'.repeat(150)}\r\n`).join('')
    const keep = async (scrollbackLimit: number) => {
      const term = new Ghostty((await WebAssembly.instantiate(shipped, { env: { log: () => {} } })).instance).createTerminal(160, 48, { scrollbackLimit })
      term.write(lines)
      return term.getScrollbackLength()
    }
    expect(await keep(10 << 20)).toBeGreaterThan((await keep(10_000)) * 2)
  })
})
