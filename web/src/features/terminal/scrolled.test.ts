// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { Ghostty, type GhosttyTerminal } from 'ghostty-web'

/**
 * Rows that scroll in at the bottom of the screen start out blank (ADR-0076). Claude Code writes a line by placing each
 * word with a column move (CHA) and skips the cells between words, which it expects to be blank. ghostty-web 0.4.0's
 * WebAssembly module cleared a row that scrolled in only when the current style had a background colour; after a style
 * reset (ESC[0m) the row kept cells of earlier output from the reused memory (upstream ghostty-web #180), and those
 * showed through the gaps of Claude Code's lines (the operator's report, 2026-11-12). The terminal loads the module of
 * a later release (ghostty.ts); 0.4.0's module is kept here as the control that shows the defect.
 */
const require = createRequire(import.meta.url)
const shipped = readFileSync(require.resolve('ghostty-web-vt/ghostty-vt.wasm'))
const v040 = readFileSync(require.resolve('ghostty-web/ghostty-vt.wasm'))
const terminal = async (wasm: typeof shipped) => new Ghostty((await WebAssembly.instantiate(wasm, { env: { log: () => {} } })).instance).createTerminal(160, 48)

const text = (t: GhosttyTerminal, y: number) => (t.getLine(y) ?? []).map((c) => (c.codepoint ? String.fromCodePoint(c.codepoint) : ' ')).join('').trimEnd()

/** What the stub harness's "/sparse <n>" prints (internal/harness/harnesstest/stubharness). */
function sparse(n: number): string {
  let out = ''
  for (let i = 1; i <= n; i++) out += `\x1b[38;2;${40 + (i % 200)};${40 + Math.floor(i / 200) % 200};${120 + (i % 97)}mlong line ${i} ${'x'.repeat(140)}\x1b[0m\r\n`
  for (let i = 1; i <= 60; i++) out += `\x1b[3Gsparse\x1b[10Gline\x1b[15G${i}\r\n`
  return out
}

/** The screen rows that are not what the program wrote. */
function stale(t: GhosttyTerminal): string[] {
  const bad: string[] = []
  for (let y = 0; y < t.rows; y++) {
    const row = text(t, y)
    if (row !== '' && !/^ {2}sparse line \d+$/.test(row)) bad.push(row)
  }
  return bad
}

describe('rows scrolled in after a style reset', () => {
  it('are blank in the module the terminal loads', async () => {
    const t = await terminal(shipped)
    t.write(sparse(1000))
    expect(stale(t)).toEqual([])
    expect(text(t, 46)).toBe('  sparse line 60')
  })

  it('kept earlier output in ghostty-web 0.4.0’s module (upstream #180)', async () => {
    const t = await terminal(v040)
    t.write(sparse(1000))
    expect(stale(t).length).toBeGreaterThan(0)
    // The earlier line shows through the cells the program skipped: "losparseeline 14xxxx…".
    expect(stale(t)[0]).toMatch(/^losparse.line \d+x{10}/)
  })
})
