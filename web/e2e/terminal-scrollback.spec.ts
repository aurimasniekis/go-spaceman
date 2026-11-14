import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * The web terminal keeps at least 10,000 rows of earlier output at the node's 160 columns. ghostty-web's `scrollback`
 * option is a byte limit, not a line count (scrollback.ts, ADR-0080): `scrollback: 10_000` kept only a few hundred
 * rows. The stub's "/sparse 12000" prints 12,000 lines of 154 characters, each in a 24-bit colour of its own (the most
 * memory a row of Claude Code's output takes), then 60 short lines and a "tick N" line a second.
 */
interface Probe {
  __smTerminals?: { texts(): string[]; screens(): string[]; wasmBytes(): number }
}

test('the terminal keeps 10,000 rows of scrollback at 160 columns', async ({ page }) => {
  test.setTimeout(240_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal scrollback journey password'
  const id = Date.now().toString(36) // unique per attempt, so the spec can run again on the same stack
  await asRoot.call('POST', '/users', { display_name: 'Scrollback Tester', email: `scrollback-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`scrollback-${id}@example.com`, password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'scrollback-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `scrollback-${id}`, name: `Scrollback ${id}`, node_id: nodeId }, { 'Idempotency-Key': `terminal-scrollback-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `scrollback-stub-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'scrollback', harness: 'claude-code', prompt: '/sparse 12000' })
  const runPath = `/spaces/${sp.id}/runs/${run.id}`
  await expect.poll(async () => (await api.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).toBe('running')
  try {
    await page.addInitScript(() => {
      ;(window as unknown as { __smTerminalProbe: boolean }).__smTerminalProbe = true
    })
    await page.setViewportSize({ width: 1800, height: 1000 })
    await page.goto(runPath)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 30_000 })
    await expect(page.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    // A tick on screen: all of the stub's output was applied.
    const screen = () => page.evaluate(() => (window as unknown as Probe).__smTerminals?.screens()[0] ?? '')
    await expect.poll(screen, { timeout: 60_000 }).toMatch(/^tick \d+$/m)
    const size = (await page.getByTestId('terminal-badge-size').first().textContent())?.split('×').map(Number) ?? []
    expect(size, 'size badge').toEqual([160, 48])

    const lines = (await page.evaluate(() => (window as unknown as Probe).__smTerminals!.texts()[0])).split('\n')
    const longLines = lines.filter((l) => /^long line \d+ x{140}$/.test(l))
    const oldest = Number(/^long line (\d+) /.exec(longLines[0] ?? '')?.[1])
    const bytes = await page.evaluate(() => (window as unknown as Probe).__smTerminals!.wasmBytes())
    console.log(`scrollback: ${lines.length - 48} rows above the screen, oldest long line ${oldest}; WebAssembly memory ${(bytes / 2 ** 20).toFixed(1)} MiB`)
    expect(lines.length - 48, 'rows above the screen').toBeGreaterThanOrEqual(10_000)
    // The rows kept are the newest, in order, none missing: the limit dropped only the oldest.
    expect(oldest, 'the oldest lines were dropped').toBeGreaterThan(1)
    expect(longLines.at(-1)).toMatch(/^long line 12000 /)
    expect(longLines.length).toBe(12_000 - oldest + 1)
  } finally {
    await api.call('POST', `${runPath}/stop`, {})
  }
})
