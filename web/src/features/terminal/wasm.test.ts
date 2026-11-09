// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { Ghostty, type GhosttyTerminal } from 'ghostty-web'

/**
 * The ghostty-web 0.4.0 behaviour createTerminal works around (ghostty.ts), against the real WebAssembly module: in one
 * shared instance, a terminal created after another was freed starts out with the freed terminal's cells, and freed
 * memory of a terminal with many styles is never reused, so the memory only grows. A terminal in an instance of its
 * own starts empty.
 */
const wasm = readFileSync(createRequire(import.meta.url).resolve('ghostty-web/ghostty-vt.wasm'))
const instance = async () => new Ghostty((await WebAssembly.instantiate(wasm, { env: { log: () => {} } })).instance)

const screen = (t: GhosttyTerminal) => {
  const rows: string[] = []
  for (let y = 0; y < t.rows; y++) rows.push((t.getLine(y) ?? []).map((c) => (c.codepoint ? String.fromCodePoint(c.codepoint) : ' ')).join('').trimEnd())
  return rows.join('\n')
}

/** A run's terminal at hc's 200×120 with a full scrollback of its output, freed the way a page leaves it. */
function viewAndLeave(g: Ghostty, out: string) {
  const t = g.createTerminal(120, 40, { scrollbackLimit: 10_000 })
  t.resize(200, 120)
  t.write(out)
  t.write('\x1bc')
  t.free()
}

const runA = Array.from({ length: 400 }, (_, i) => `run A secret line ${i}\r\n`).join('')
const styled = Array.from({ length: 12_000 }, (_, i) => `\x1b[38;2;10;${i % 255};20mline ${i}\x1b[0m\r\n`).join('')

function nextTerminal(g: Ghostty) {
  const t = g.createTerminal(120, 40, { scrollbackLimit: 10_000 })
  t.resize(200, 120)
  t.write('run B\r\n')
  return t
}

describe('ghostty-web 0.4.0 WebAssembly instances', () => {
  it('leave a freed terminal’s text to the next terminal of a shared instance', async () => {
    const g = await instance()
    viewAndLeave(g, runA)
    expect(screen(nextTerminal(g))).toContain('run A secret line')
  })

  it('give a terminal of its own instance an empty screen', async () => {
    viewAndLeave(await instance(), runA)
    const b = nextTerminal(await instance())
    expect(screen(b)).not.toContain('run A')
    expect(screen(b)).toContain('run B')
  })

  it('grow a shared instance with every styled terminal viewed', async () => {
    const g = await instance()
    viewAndLeave(g, styled)
    const after1 = (g as unknown as { memory: WebAssembly.Memory }).memory.buffer.byteLength
    viewAndLeave(g, styled)
    viewAndLeave(g, styled)
    expect((g as unknown as { memory: WebAssembly.Memory }).memory.buffer.byteLength).toBeGreaterThan(after1 * 2)
  })
})
