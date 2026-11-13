import { expect, test, type CDPSession, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * Clicking Terminals or Spaces in the sidebar on a cold cache froze the whole tab for about a minute on a desktop and
 * minutes on a phone (operator report, 2026-11-12, v0.1.1: the freeze followed the click, not a page load). A run's live
 * terminal first receives its stored output, tens of megabytes for a long session; it was applied record by record in
 * the tasks that delivered it, a replay applied it in one task, and every frame redrew the screen at a cost of
 * rows × rows × columns (ghostty-web 0.4.0). In a fresh browser context (an empty cache) with the CPU slowed 4×
 * (DevTools' mid-tier mobile setting), starting from the home page:
 * - from a click on Terminals until the live terminal has loaded, no task is longer than ROUTE_TASK_MS: the longest is
 *   the route's own first React render, about 330 ms here (before the fix: up to 1.0 s, 10.5 s of blocked page in all);
 * - while the terminal catches up, live or replayed, no task is longer than LONG_TASK_MS (before: up to 0.9 s live and
 *   1.5 s replayed, 5.7 s at 6×);
 * - a key press is handled within INPUT_MS meanwhile (before: 1.9-2.8 s). Throttling pauses the main thread in slices
 *   of its own, so a key's round trip runs a few hundred milliseconds past the longest task;
 * - a click on Spaces runs no task longer than PAGE_TASK_MS: React rendering the page, about 140-190 ms here before
 *   and after the fix.
 * The cache is not disabled through DevTools: its Network domain instruments every request (an initiator stack trace
 * per fetch), which at 4× made the page's own refetches a 2 s task that browsers never run.
 */
// SM_E2E_CPU_THROTTLE measures at another rate (the bounds are for 4×); SM_E2E_CACHE_DISABLED=1 also disables the cache.
const THROTTLE = Number(process.env.SM_E2E_CPU_THROTTLE ?? 4)
const LONG_TASK_MS = 200
const INPUT_MS = 1_000
const PAGE_TASK_MS = 300
const ROUTE_TASK_MS = 500
/** How long the home page gets to settle (its own render and queries) before a sidebar item is clicked. */
const SETTLE_MS = 4_000

const sum = (ms: number[]) => ms.reduce((n, d) => n + d, 0)

// Playwright's trace snapshots the page's DOM on its main thread for every action, which would be measured too.
test.use({ trace: 'off' })

interface Probe {
  __smTerminals?: { count(): number; screens(): string[] }
  __smLongTasks: { start: number; duration: number }[]
}

/** The page's long tasks, recorded by the page itself. */
function observe() {
  const w = window as unknown as Probe & { __smTerminalProbe: boolean }
  w.__smTerminalProbe = true
  w.__smLongTasks = []
  new PerformanceObserver((list) => {
    for (const e of list.getEntries()) w.__smLongTasks.push({ start: e.startTime, duration: e.duration })
  }).observe({ type: 'longtask', buffered: true })
}

const now = (page: Page) => page.evaluate(() => performance.now())
const screens = (page: Page) => page.evaluate(() => (window as unknown as Probe).__smTerminals?.screens().join('\n') ?? '')
const terminalCount = (page: Page) => page.evaluate(() => (window as unknown as Probe).__smTerminals?.count() ?? 0)

/** The long tasks that started at or after `from` (page time), longest first. */
async function longTasksSince(page: Page, from: number) {
  const all = await page.evaluate(() => (window as unknown as Probe).__smLongTasks)
  return all.filter((t) => t.start >= from).map((t) => Math.round(t.duration)).sort((a, b) => b - a)
}

/**
 * Presses a key every 250 ms until `done` resolves, and returns the longest a press took to be handled (Playwright's
 * press resolves once the page has processed the key, so a blocked main thread holds it up).
 */
async function inputDelayWhile(page: Page, done: Promise<unknown>) {
  let finished = false
  const delays: number[] = []
  const presses = (async () => {
    while (!finished) {
      const t0 = Date.now()
      await page.keyboard.press('Shift')
      delays.push(Date.now() - t0)
      await page.waitForTimeout(250)
    }
  })()
  try {
    await done
  } finally {
    finished = true
    await presses
  }
  expect(delays.length, 'key presses handled').toBeGreaterThan(0)
  return Math.max(...delays)
}

test('terminals and spaces stay responsive on a cold cache', async ({ page, browser }) => {
  test.setTimeout(300_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal load journey password'
  const id = Date.now().toString(36) // unique per attempt, so the spec can run again on the same stack
  await asRoot.call('POST', '/users', { display_name: 'Load Tester', email: `load-${id}@example.com`, password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn(`load-${id}@example.com`, password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'load-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  // Named to come first, so the Terminals page opens this space.
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: `loading-${id}`, name: `0 Loading ${id}`, node_id: nodeId }, { 'Idempotency-Key': `terminal-load-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `load-stub-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  /** Starts a run that prints `lines` lines in 24-bit colours (a long Claude Code session's worth of output). */
  const started: string[] = []
  const startRun = async (role: string, lines: number) => {
    const prompt = Array.from({ length: Math.ceil(lines / 20_000) }, () => `/styled ${Math.min(lines, 20_000)}`).join('\n')
    const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role, harness: 'claude-code', prompt })
    started.push(run.id)
    return run.id
  }
  /** Waits until the run's recording holds its whole reply: the stub ticks once it has printed it. */
  const replied = async (runId: string) => {
    let after = 0
    await expect.poll(async () => {
      for (;;) {
        const page = await api.call<{ records: { k: string; d?: string }[]; more: boolean; next_after: number }>('GET', `/spaces/${sp.id}/runs/${runId}/recording?after=${after}&limit=1000`)
        if (page.records.some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('latin1').includes('tick'))) return true
        if (page.next_after) after = page.next_after
        if (!page.more) return false
      }
    }, { timeout: 120_000, intervals: [2_000] }).toBe(true)
  }
  try {
    // About 5 MB of output each: the live terminal's catch-up, and a finished run's replay.
    const live = await startRun('load-live', 120_000)
    const done = await startRun('load-done', 120_000)
    await replied(live)
    await replied(done)
    await api.call('POST', `/spaces/${sp.id}/runs/${done}/stop`, {})
    await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${done}`)).observed, { timeout: 30_000 }).not.toBe('running')

    /**
     * A fresh browser context (an empty cache), signed in, with the CPU slowed down, on the home page once it has
     * settled: the operator's freeze followed a click on a sidebar item there (a client-side navigation that downloads
     * and runs the route's chunks), not a page load.
     */
    const coldHome = async () => {
      const ctx = await browser.newContext({ storageState: await page.context().storageState() })
      const p = await ctx.newPage()
      await p.addInitScript(observe)
      const cdp: CDPSession = await ctx.newCDPSession(p)
      if (process.env.SM_E2E_CACHE_DISABLED) {
        await cdp.send('Network.enable')
        await cdp.send('Network.setCacheDisabled', { cacheDisabled: true })
      }
      await cdp.send('Emulation.setCPUThrottlingRate', { rate: THROTTLE })
      await p.goto('/')
      const nav = (name: string) => p.getByRole('link', { name, exact: true }).first()
      await expect(nav('Terminals')).toBeVisible({ timeout: 60_000 })
      await p.waitForTimeout(SETTLE_MS)
      return { ctx, p, nav }
    }

    // Terminals, clicked on the home page: the route's chunks and first render, then the live terminal of the space's
    // running agent catching up on its stored output.
    let { ctx, p, nav } = await coldHome()
    const clicked = await now(p)
    await nav('Terminals').click()
    let opened = clicked
    let loaded = (async () => {
      await expect.poll(() => terminalCount(p), { timeout: 60_000 }).toBe(1)
      opened = await now(p)
      // The stub ticks once it has printed its reply: a tick on screen means the whole stored output was applied.
      await expect.poll(() => screens(p), { timeout: 120_000, intervals: [500] }).toMatch(/\btick \d+/)
    })()
    let delay = await inputDelayWhile(p, loaded)
    await expect(p.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    let tasks = await longTasksSince(p, clicked)
    const catchUp = await longTasksSince(p, opened)
    console.log(
      `terminals (click on home, cold cache, ${THROTTLE}× CPU): loaded in ${Math.round((await now(p)) - clicked)} ms, ${tasks.length} long tasks ` +
        `(${sum(tasks)} ms), longest ${tasks.slice(0, 5).join(', ')} ms; catching up, longest ${catchUp.slice(0, 5).join(', ')} ms; input delay ≤ ${delay} ms`,
    )
    expect.soft(tasks[0] ?? 0, 'longest task from the click until the live terminal has loaded (ms)').toBeLessThanOrEqual(ROUTE_TASK_MS)
    expect.soft(catchUp[0] ?? 0, 'longest task while the live terminal catches up (ms)').toBeLessThanOrEqual(LONG_TASK_MS)
    expect.soft(delay, 'input delay from the click until the live terminal has loaded (ms)').toBeLessThanOrEqual(INPUT_MS)
    await expect(p.getByText('Live · watch-only')).toBeVisible()

    // The finished run: its replay renders the whole recording.
    // (An in-app navigation, as from the run's link on the space overview.)
    let from = await now(p)
    await p.evaluate((url) => {
      history.pushState({}, '', url)
      dispatchEvent(new PopStateEvent('popstate', { state: {} }))
    }, `/terminals?space=${sp.id}&run=${done}`)
    await expect(p.getByTestId('replay-toolbar')).toBeVisible({ timeout: 30_000 })
    loaded = expect.poll(() => screens(p), { timeout: 120_000, intervals: [500] }).toMatch(/\btick \d+/)
    delay = await inputDelayWhile(p, loaded)
    await expect(p.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    tasks = await longTasksSince(p, from)
    console.log(`terminals (replay): loaded in ${Math.round((await now(p)) - from)} ms, ${tasks.length} long tasks, longest ${tasks.slice(0, 5).join(', ')} ms; input delay ≤ ${delay} ms`)
    expect.soft(tasks[0] ?? 0, 'longest task while the replay renders (ms)').toBeLessThanOrEqual(LONG_TASK_MS)
    expect.soft(delay, 'input delay while the replay renders (ms)').toBeLessThanOrEqual(INPUT_MS)

    await ctx.close()

    // Spaces, clicked on the home page of another fresh context.
    ;({ ctx, p, nav } = await coldHome())
    from = await now(p)
    await nav('Spaces').click()
    loaded = (async () => {
      await expect(p.getByRole('link', { name: `0 Loading ${id}` })).toBeVisible({ timeout: 30_000 })
      await p.waitForTimeout(2_000)
    })()
    delay = await inputDelayWhile(p, loaded)
    tasks = await longTasksSince(p, from)
    console.log(`spaces (click on home): ${tasks.length} long tasks, longest ${tasks.slice(0, 5).join(', ')} ms; input delay ≤ ${delay} ms`)
    expect.soft(tasks[0] ?? 0, 'longest task while Spaces opens (ms)').toBeLessThanOrEqual(PAGE_TASK_MS)
    expect.soft(delay, 'input delay while Spaces opens (ms)').toBeLessThanOrEqual(INPUT_MS)
    await ctx.close()
  } finally {
    for (const id of started) await api.call('POST', `/spaces/${sp.id}/runs/${id}/stop`, {}).catch(() => {})
  }
})
