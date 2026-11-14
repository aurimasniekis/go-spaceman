// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { Ghostty, Terminal } from 'ghostty-web'
import { SCROLLBACK_BYTES, scrollbackRows } from './scrollback'

/**
 * The scrollback limit is in bytes (scrollback.ts), measured against the WebAssembly module the page loads: the
 * setting keeps at least 10,000 rows at the node's 160 columns, also when every row has a 24-bit colour of its own (the
 * costliest rows measured, like the stub's "/sparse" lines), and at least scrollbackRows(cols) at every width.
 */
const require = createRequire(import.meta.url)
const shipped = readFileSync(require.resolve('ghostty-web-vt/ghostty-vt.wasm'))
const instance = async () => new Ghostty((await WebAssembly.instantiate(shipped, { env: { log: () => {} } })).instance)

const plain = (cols: number) => (i: number) => `${i} ${'x'.repeat(cols - 12)}\r\n`
const coloured = (cols: number) => (i: number) =>
  `\x1b[38;2;${40 + (i % 200)};${40 + (Math.floor(i / 200) % 200)};${120 + (i % 97)}m${i} ${'x'.repeat(cols - 12)}\x1b[0m\r\n`

/** Writes n lines in 64 kB writes and returns the rows kept above the screen and the instance's memory in bytes. */
async function fill(cols: number, line: (i: number) => string, n: number, scrollbackLimit = SCROLLBACK_BYTES) {
  const g = await instance()
  const t = g.createTerminal(cols, 48, { scrollbackLimit })
  let chunk = ''
  for (let i = 0; i < n; i++) {
    chunk += line(i)
    if (chunk.length > 64 << 10) {
      t.write(chunk)
      chunk = ''
    }
  }
  t.write(chunk)
  const rows = t.getScrollbackLength()
  const text = (y: number) => (t.getScrollbackLine(y) ?? []).map((c) => (c.codepoint ? String.fromCodePoint(c.codepoint) : ' ')).join('').trimEnd()
  const first = text(0)
  t.free()
  return { rows, first, bytes: (g as unknown as { memory: WebAssembly.Memory }).memory.buffer.byteLength }
}

describe('the terminal scrollback', () => {
  it('passes through ghostty-web 0.4.0’s JavaScript unchanged, as the module’s byte limit', async () => {
    const term = new Terminal({ ghostty: await instance(), scrollback: SCROLLBACK_BYTES }) as unknown as { buildWasmConfig(): { scrollbackLimit: number } }
    expect(term.buildWasmConfig().scrollbackLimit).toBe(SCROLLBACK_BYTES)
  })

  it('counts bytes, not lines: the old setting of 10,000 kept a few hundred rows at 160 columns', async () => {
    const { rows } = await fill(160, plain(160), 2_000, 10_000)
    expect(rows).toBeLessThan(1_000)
  })

  it('keeps at least 10,000 rows at 160 columns, plain or in per-row 24-bit colours, and drops the oldest beyond', async () => {
    expect(scrollbackRows(160)).toBeGreaterThanOrEqual(10_000)
    for (const line of [plain(160), coloured(160)]) {
      const { rows, first } = await fill(160, line, 14_000)
      expect(rows).toBeGreaterThanOrEqual(10_000)
      expect(rows).toBeLessThan(14_000 - 47) // the limit holds: the oldest lines are gone
      expect(first).toMatch(new RegExp(`^${14_000 - 47 - rows} x+$`)) // and the rows kept are the newest ones
    }
  })

  it('keeps at least scrollbackRows(cols) at 80, 160 and 200 columns', async () => {
    for (const cols of [80, 160, 200]) {
      for (const line of [plain(cols), coloured(cols)]) {
        const { rows } = await fill(cols, line, scrollbackRows(cols) + 2_000)
        expect(rows, `${cols} columns`).toBeGreaterThanOrEqual(scrollbackRows(cols))
      }
    }
  })

  it('holds a full scrollback of plain text in about 31 MB of WebAssembly memory at 160 columns', async () => {
    const { bytes } = await fill(160, plain(160), 14_000)
    expect(bytes).toBeLessThanOrEqual(40 << 20)
  })
})
