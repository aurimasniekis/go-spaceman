// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { Ghostty, GhosttyTerminal } from 'ghostty-web'
import { cacheViewport } from './ghostty'

/**
 * The viewport cache that makes a redraw parse the screen once (cacheViewport), on ghostty-web 0.4.0 with the real
 * module the terminal loads: every row reads the same as without it, a write or a resize is seen at once, and a full
 * redraw parses the viewport once instead of once per row.
 */
const wasm = readFileSync(createRequire(import.meta.url).resolve('ghostty-web-vt/ghostty-vt.wasm'))
const instance = async () => new Ghostty((await WebAssembly.instantiate(wasm, { env: { log: () => {} } })).instance)
const proto = GhosttyTerminal.prototype as unknown as { parseCellsIntoPool(...a: unknown[]): void }

const text = (t: GhosttyTerminal, y: number) => (t.getLine(y) ?? []).map((c) => (c.codepoint ? String.fromCodePoint(c.codepoint) : ' ')).join('').trimEnd()
const screen = (t: GhosttyTerminal) => Array.from({ length: t.rows }, (_, y) => text(t, y))
const styled = (from: number, n: number) => Array.from({ length: n }, (_, i) => `\x1b[38;2;10;${(from + i) % 255};20mline ${from + i}\x1b[0m\r\n`).join('')

describe('cacheViewport', () => {
  it('parses the viewport once per redraw and never returns stale rows', async () => {
    const parses = vi.spyOn(proto, 'parseCellsIntoPool')
    const before = (await instance()).createTerminal(200, 120)
    before.write(styled(0, 300))
    parses.mockClear()
    const plain = screen(before)
    expect(parses).toHaveBeenCalledTimes(120) // ghostty-web 0.4.0: the whole viewport for every row

    cacheViewport()
    const t = (await instance()).createTerminal(200, 120)
    t.write(styled(0, 300))
    parses.mockClear()
    expect(screen(t)).toEqual(plain)
    expect(parses).toHaveBeenCalledTimes(1)
    // Cells come back as copies: changing one never changes the cache.
    t.getLine(0)![0].codepoint = 88
    expect(text(t, 0)).toBe(plain[0])

    // A write, a resize, or a later task reads the terminal again.
    t.write('\x1b[H\x1b[2Kchanged')
    expect(text(t, 0)).toBe('changed')
    t.resize(100, 50)
    expect(screen(t)).toHaveLength(50)
    expect(text(t, 0).length).toBeLessThanOrEqual(100)
    parses.mockClear()
    await Promise.resolve()
    text(t, 1)
    expect(parses).toHaveBeenCalledTimes(1)
    parses.mockRestore()
  })
})
