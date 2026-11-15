import { expect, test, type Browser, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * Old output must never show through new lines (operator report, 2026-11-12, v0.1.1): Claude Code writes a line by
 * placing each word with a column move and skips the cells between words, expecting the row that has just scrolled in
 * to be blank. ghostty-web 0.4.0's WebAssembly module left earlier output in such rows after a style reset (upstream
 * #180, ADR-0076), so lines read like "losparseeline 14xxxx…". The stub harness's "/sparse 1000" prints that pattern:
 * 1000 long lines in 24-bit colours, each ending in a reset, then 60 short lines placed with column moves, then a
 * "tick N" line a second.
 *
 * For a live terminal and its replay, at device pixel ratios 1 and 2, with the node's default 160×48 shown at its own
 * size and scaled down to fit a narrow window, every row reads exactly what the program wrote (the terminal's own
 * cells), and the canvas has nothing but background where a row has no text.
 */
interface Probe {
  __smTerminals?: { screens(): string[] }
}

/** Whether a row is one the stub printed, blank, or the terminal's echo of the stop (^C). */
const expected = (row: string) => row === '' || row === '^C' || /^ {2}sparse line \d+$/.test(row) || /^tick \d+$/.test(row) || /^long line \d+ x{140}$/.test(row)

/** The start of a row that the stub wrote; everything after it must be blank. */
const written = (row: string) => /^(\^C| {2}sparse line \d+|tick \d+|long line \d+ x{140})?/.exec(row)?.[0] ?? ''

/** The terminal's rows, read from its cells. */
const rows = async (page: Page) => (await page.evaluate(() => (window as unknown as Probe).__smTerminals?.screens()[0] ?? '')).split('\n')

/**
 * The cells of the canvas that should be blank (after each row's text, up to the last two columns, where the scrollbar
 * is drawn) but have ink: pixels further than a small tolerance from the theme's background, Night Owl #011627.
 */
async function inkedBlankCells(page: Page, text: string[], cols: number, skipRow: number) {
  return page.evaluate(
    ({ text, cols, skipRow }) => {
      const canvas = document.querySelector('[data-testid="terminal"] canvas') as HTMLCanvasElement | null
      const ctx = canvas?.getContext('2d')
      if (!canvas || !ctx) return ['no canvas']
      const cellW = canvas.width / cols
      const rowH = canvas.height / text.length
      const bad: string[] = []
      text.forEach((row, y) => {
        if (y === skipRow) return // the cursor's block
        const from = Math.ceil((row.length + 1) * cellW)
        const to = Math.floor((cols - 2) * cellW)
        if (to <= from) return
        const top = Math.ceil(y * rowH) + 1
        const h = Math.floor((y + 1) * rowH) - top - 1
        if (h <= 0) return
        const px = ctx.getImageData(from, top, to - from, h).data
        for (let i = 0; i < px.length; i += 4) {
          if (Math.abs(px[i] - 1) + Math.abs(px[i + 1] - 22) + Math.abs(px[i + 2] - 39) > 24) {
            const x = Math.floor(((i / 4) % (to - from) + from) / cellW)
            bad.push(`row ${y} col ${x}: ${JSON.stringify(row)}`)
            return
          }
        }
      })
      return bad
    },
    { text, cols, skipRow },
  )
}

/** Opens the run in a fresh context and checks its terminal once the program's output is on screen. */
async function check(browser: Browser, page: Page, url: string, opts: { dpr: number; width: number; label: string; replay: boolean }) {
  const ctx = await browser.newContext({ storageState: await page.context().storageState(), deviceScaleFactor: opts.dpr, viewport: { width: opts.width, height: 1000 } })
  try {
    const p = await ctx.newPage()
    await p.addInitScript(() => {
      ;(window as unknown as { __smTerminalProbe: boolean }).__smTerminalProbe = true
    })
    await p.goto(url)
    if (opts.replay) await expect(p.getByTestId('replay-toolbar')).toBeVisible({ timeout: 30_000 })
    else await expect(p.getByText('Live · watch-only')).toBeVisible({ timeout: 30_000 })
    await expect(p.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    // The stub ticks once a second after its output (each tick also ends in a reset and scrolls a row in), so the
    // sparse lines scroll away after a while: a tick on screen means all of the output was applied.
    await expect.poll(async () => (await rows(p)).join('\n'), { timeout: 60_000 }).toMatch(/^tick \d+$/m)
    const size = (await p.getByTestId('terminal-badge-size').first().textContent())?.split('×').map(Number) ?? []
    expect(size, `${opts.label}: size badge`).toEqual([160, 48])
    // Let a frame draw what the cells hold.
    await p.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))))
    const text = await rows(p)
    expect(text, `${opts.label}: every row matches the stub's output`).toHaveLength(48)
    expect(text.filter((r) => !expected(r)), `${opts.label}: rows with earlier output showing through`).toEqual([])
    const canvas = p.locator('[data-testid="terminal"] canvas')
    const shown = await canvas.boundingBox()
    console.log(`${opts.label}: canvas ${await canvas.evaluate((c: HTMLCanvasElement) => `${c.width}×${c.height}`)} px shown at ${Math.round(shown?.width ?? 0)}×${Math.round(shown?.height ?? 0)}`)
    const cursorRow = text.findLastIndex((r) => r !== '') + 1
    // Measured from what the stub wrote, not from the cells: the canvas must show the program's output alone.
    expect(await inkedBlankCells(p, text.map(written), 160, cursorRow), `${opts.label}: ink in blank cells`).toEqual([])
    return shown?.width ?? 0
  } finally {
    await ctx.close()
  }
}

test('old output never shows through new lines', async ({ page, browser }) => {
  test.setTimeout(240_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal stale journey password'
  const id = Date.now().toString(36) // unique per attempt, so the spec can run again on the same stack
  await asRoot.call('POST', '/users', { display_name: 'Stale Tester', email: `stale-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`stale-${id}@example.com`, password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'stale-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `stale-${id}`, name: `Stale ${id}`, node_id: nodeId }, { 'Idempotency-Key': `terminal-stale-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `stale-stub-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'sparse', harness: 'claude-code', prompt: '/sparse 1000' })
  const runPath = `/spaces/${sp.id}/runs/${run.id}`
  await expect.poll(async () => (await api.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).toBe('running')
  try {
    // At its own size the 160 columns fit a wide window; in a narrow one the font shrinks to fit them (fitFont).
    for (const dpr of [1, 2]) {
      const wide = await check(browser, page, runPath, { dpr, width: 1800, label: `live, DPR ${dpr}, 1800 px`, replay: false })
      const narrow = await check(browser, page, runPath, { dpr, width: 900, label: `live, DPR ${dpr}, 900 px`, replay: false })
      expect(narrow, 'the narrow window scales the terminal down').toBeLessThan(wide * 0.8)
    }
  } finally {
    await api.call('POST', `${runPath}/stop`, {})
  }
  await expect.poll(async () => (await api.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).not.toBe('running')
  for (const dpr of [1, 2]) {
    for (const width of [1800, 900]) await check(browser, page, `${runPath}?view=replay`, { dpr, width, label: `replay, DPR ${dpr}, ${width} px`, replay: true })
  }
})
