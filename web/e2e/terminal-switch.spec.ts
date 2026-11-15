import { expect, test, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/** What the page exposes to this test (installed by addInitScript below and by the terminal module's probe). */
interface Probe {
  __smTerminals?: { count(): number; texts(): string[]; wasmBytes(): number }
  __smStreams: { open: number; opened: number }
}

/** The terminals open on the page and the text of each (the canvas has no text of its own). */
const terminals = (page: Page) => page.evaluate(() => (window as unknown as Probe).__smTerminals?.texts() ?? [])
const openStreams = (page: Page) => page.evaluate(() => (window as unknown as Probe).__smStreams.open)
const responds = (page: Page) => Promise.race([page.evaluate(() => 'responds'), new Promise((r) => setTimeout(() => r('hung'), 5_000))])

/**
 * Switching between agents' terminals in the Terminals view (operator reports, 2026-11-08): a few quick switches froze
 * the tab on "Connecting…" and crashed it, and a terminal showed lines of a terminal viewed earlier, interleaved with
 * its own. Each switch must leave exactly one terminal and one stream open, the page responsive, the shared WebAssembly
 * memory bounded, and the new terminal showing only its own run's output, right on first open.
 */
test('switching between run terminals', async ({ page }) => {
  test.setTimeout(180_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal switch journey password'
  await asRoot.call('POST', '/users', { display_name: 'Switch Tester', email: 'switch@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('switch@example.com', password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'switch-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'switching', name: 'Switching', node_id: nodeId }, { 'Idempotency-Key': 'terminal-switch-space' })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: 'switch-stub', kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  const started: string[] = []
  const startRun = async (role: string, prompt: string, marker: string) => {
    const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role, harness: 'claude-code', prompt })
    started.push(run.id)
    await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${run.id}`)).observed, { timeout: 30_000 }).toBe('running')
    await expect.poll(async () => {
      const rec = await api.call<{ records: { k: string; d?: string }[] }>('GET', `/spaces/${sp.id}/runs/${run.id}/recording`)
      return rec.records.some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('latin1').includes(marker))
    }, { timeout: 20_000 }).toBe(true)
    return run.id
  }
  try {
    // Output in many colours (a full scrollback, like Claude Code's ordinary output) and then Claude Code's full-screen
    // start-up (alternate screen); a long plain reply; and a short one. Each has text the others never print.
    await startRun('switch-full', '/styled 8000\n/fullscreen', '[?1049h')
    await startRun('switch-long', '/lines 400', 'reply line 400 of 400')
    const short = await startRun('switch-short', '/lines 30', 'reply line 30 of 30')
    const own: Record<string, string> = { 'switch-full': 'permission-modes', 'switch-long': 'reply line 400 of 400', 'switch-short': 'reply line 30 of 30' }
    const foreign: Record<string, string[]> = {
      'switch-full': ['reply line'],
      'switch-long': ['permission-modes', 'styled line', ' of 30\n'],
      'switch-short': ['permission-modes', ' of 400', 'styled line'],
    }

    // Ask the terminal module for its probe, and count the terminal streams the page opens and closes.
    await page.addInitScript(() => {
      const w = window as unknown as { __smTerminalProbe: boolean; __smStreams: { open: number; opened: number } }
      w.__smTerminalProbe = true
      w.__smStreams = { open: 0, opened: 0 }
      const Orig = window.EventSource
      class Counted extends Orig {
        private counted: boolean
        constructor(url: string | URL, init?: EventSourceInit) {
          super(url, init)
          this.counted = String(url).includes('/terminal')
          if (!this.counted) return
          w.__smStreams.open++
          w.__smStreams.opened++
          this.addEventListener('error', () => this.readyState === Orig.CLOSED && this.release())
        }
        private release() {
          if (this.counted) w.__smStreams.open--
          this.counted = false
        }
        close() {
          this.release()
          super.close()
        }
      }
      window.EventSource = Counted
    })

    // First open, without a refresh: the terminal draws its run's output at once.
    await page.goto(`/spaces/${sp.id}/terminals?run=${short}`)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
    await expect.poll(async () => (await terminals(page)).join(''), { timeout: 10_000 }).toContain(own['switch-short'])

    const tab = (role: string) => page.getByRole('tablist', { name: 'Agents' }).getByRole('tab', { name: new RegExp(role) })
    // One terminal, live, with its own run's output and nothing of the others'.
    const show = async (role: string) => {
      await tab(role).click()
      await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 15_000 })
      await expect.poll(async () => (await terminals(page)).join(''), { timeout: 10_000 }).toContain(own[role])
      const texts = await terminals(page)
      expect(texts).toHaveLength(1)
      for (const other of foreign[role]) expect.soft(texts[0], `${role} shows "${other.trim()}" of another run`).not.toContain(other)
    }
    const wasmBytes = () => page.evaluate(() => (window as unknown as Probe).__smTerminals!.wasmBytes())
    // The previous terminal's output never shows in the next one. The WebAssembly memory each terminal first needs is
    // the baseline for later.
    const first: Record<string, number> = {}
    for (const role of ['switch-long', 'switch-short', 'switch-full']) {
      await show(role)
      first[role] = await wasmBytes()
    }
    await show('switch-long')
    await show('switch-full')
    await show('switch-short')

    // 20 quick switches, without waiting for each terminal to connect.
    const order = ['switch-long', 'switch-full', 'switch-short']
    for (let i = 0; i < 20; i++) {
      await tab(order[i % 3]).click()
      await page.waitForTimeout(i % 4 === 0 ? 50 : 250)
      if (i % 5 === 4) expect(await responds(page), `after ${i + 1} switches`).toBe('responds')
    }
    // Then each again: one terminal, one stream, the WebAssembly memory of that one terminal (the previous terminals'
    // memory was given back), and only its own output.
    for (const role of order) {
      await show(role)
      await expect.poll(() => openStreams(page), { timeout: 5_000 }).toBe(1)
      expect(await page.evaluate(() => (window as unknown as Probe).__smTerminals!.count())).toBe(1)
      expect(await wasmBytes(), `${role}'s WebAssembly memory`).toBeLessThanOrEqual(first[role] * 2)
    }
    expect(await responds(page)).toBe('responds')
  } finally {
    for (const id of started) await api.call('POST', `/spaces/${sp.id}/runs/${id}/stop`, {}).catch(() => {})
  }
})
