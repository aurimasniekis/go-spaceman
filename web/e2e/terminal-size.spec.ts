import { expect, test, type Page } from '@playwright/test'
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { Api, env, settle, startAgent } from './stack'

/**
 * Resizing a live run's terminal (ADR-0075) against the real stack: central, a real spaceman-agent and its own
 * herdr-core core. The stub harness's "/size" line draws a full-width rule and its PTY size whenever the terminal is
 * resized (SIGWINCH), so the recording shows that the program really redrew at each size. An admin picks a preset, a
 * custom size, fit to window and the node default; an operator (no terminal takeover) sees the control disabled; a
 * viewer sees none. Screenshots at 390 and 1280 px go to SM_SHOTS (for review, not committed here).
 */
const shots = process.env.SM_SHOTS ?? join(process.cwd(), 'test-results', 'shots')

async function shot(page: Page, name: string) {
  mkdirSync(shots, { recursive: true })
  await settle(page)
  await page.screenshot({ path: join(shots, `${name}.png`), animations: 'disabled' })
}

type Rec = { k: string; d?: string; cols?: number; rows?: number }

test('resizing a live run terminal', async ({ page, browser, baseURL }) => {
  test.setTimeout(180_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal size journey password'
  await asRoot.call('POST', '/users', { display_name: 'Size Admin', email: 'size-admin@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('size-admin@example.com', password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'size-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'termsize', name: 'Terminal size', node_id: nodeId }, { 'Idempotency-Key': 'terminal-size-space' })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: 'termsize-stub', kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)
  const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'sizer', harness: 'claude-code', prompt: '/size' })
  const runPath = `/spaces/${sp.id}/runs/${run.id}`
  await expect.poll(async () => (await api.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).toBe('running')

  const records = async () => (await api.call<{ records: Rec[] }>('GET', `${runPath}/recording?limit=5000`)).records
  const drewAt = async (cols: number, rows: number) => {
    const recs = await records()
    const sized = recs.findIndex((r) => r.k === 'size' && r.cols === cols && r.rows === rows)
    // The program redrew at the new size after the size record (its SIGWINCH handler reads TIOCGWINSZ).
    return sized >= 0 && recs.slice(sized).some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('utf8').includes(`terminal size ${cols}x${rows}`))
  }
  const openSize = async () => {
    const open = (await records()).find((r) => r.k === 'open')
    return `${open?.cols}x${open?.rows}`
  }
  await expect.poll(openSize, { timeout: 20_000 }).not.toBe('undefinedxundefined')
  const start = await openSize()

  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto(runPath)
  await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
  const control = page.getByTestId('terminal-size')
  await expect(control).toHaveText(/Node default/)
  await expect(control).toBeEnabled()
  await expect(control).not.toHaveAttribute('aria-disabled')

  // A preset: the node holds the PTY at it and the program redraws for it.
  await control.click()
  await shot(page, 'terminal-size-menu-1280')
  await page.getByTestId('terminal-size-120x40').click()
  await expect(control).toHaveText(/120×40/)
  await expect.poll(() => drewAt(120, 40), { timeout: 20_000 }).toBe(true)
  await expect(page.getByTestId('terminal-badge-size')).toHaveText('120×40') // the viewer's badge follows the recording

  // A custom size, bounded in the form.
  await control.click()
  await page.getByTestId('terminal-size-custom').click()
  await page.getByTestId('terminal-size-cols').fill('30')
  await page.getByTestId('terminal-size-rows').fill('20')
  await expect(page.getByTestId('terminal-size-apply')).toBeDisabled()
  await page.getByTestId('terminal-size-cols').fill('100')
  await page.getByTestId('terminal-size-rows').fill('30')
  await shot(page, 'terminal-size-custom-1280')
  await page.getByTestId('terminal-size-apply').click()
  await expect(control).toHaveText(/100×30/)
  await expect.poll(() => drewAt(100, 30), { timeout: 20_000 }).toBe(true)

  // Fit to window: a size from the terminal area at the regular font size; it refits when the window changes.
  await control.click()
  await page.getByTestId('terminal-size-fit').click()
  await expect(control).toHaveText(/· fit/)
  const fitted = async () => (await api.call<{ terminal_size?: { cols: number; rows: number } }>('GET', runPath)).terminal_size
  await expect.poll(async () => (await fitted())?.cols ?? 0, { timeout: 10_000 }).toBeGreaterThan(40)
  const wide = (await fitted())!
  await expect.poll(() => drewAt(wide.cols, wide.rows), { timeout: 20_000 }).toBe(true)
  await shot(page, 'terminal-size-fit-1280')
  await page.setViewportSize({ width: 900, height: 900 })
  await expect.poll(async () => (await fitted())?.cols ?? 0, { timeout: 10_000 }).toBeLessThan(wide.cols)
  // The same window always fits to the same size.
  const narrow = (await fitted())!
  await page.setViewportSize({ width: 1280, height: 900 })
  await expect.poll(fitted, { timeout: 10_000 }).toEqual(wide)
  await page.setViewportSize({ width: 900, height: 900 })
  await expect.poll(fitted, { timeout: 10_000 }).toEqual(narrow)

  // Someone else resizes while this browser is on Fit: their size stands. B, a second admin in another browser with
  // the run open, sets 80x24 through the API; A stops fitting and shows it, even when its terminal box changes size
  // with the run's new size, and only A's own choice brings Fit back.
  await asRoot.call('POST', '/users', { display_name: 'Size Admin B', email: 'size-admin-b@example.com', password, grants: [{ role: 'admin', domain: sp.id }] })
  const ctxB = await browser.newContext({ baseURL })
  const pageB = await ctxB.newPage()
  const apiB = new Api(pageB.request, base)
  await apiB.signIn('size-admin-b@example.com', password)
  await pageB.goto(runPath)
  await expect(pageB.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
  await apiB.call('PUT', `${runPath}/terminal-size`, { cols: 80, rows: 24 })
  await expect(control).toHaveText(/^80×24$/, { timeout: 10_000 })
  await expect(pageB.getByTestId('terminal-size')).toHaveText(/80×24/)
  await page.waitForTimeout(2_500) // longer than the refit debounce and the box's resize
  expect(await fitted()).toEqual({ cols: 80, rows: 24 })
  await page.setViewportSize({ width: 1000, height: 900 })
  await page.waitForTimeout(1_000)
  expect(await fitted()).toEqual({ cols: 80, rows: 24 })
  await control.click()
  await expect(page.getByTestId('terminal-size-80x24')).toHaveAttribute('aria-checked', 'true')
  await expect(page.getByTestId('terminal-size-fit')).toHaveAttribute('aria-checked', 'false')
  await page.keyboard.press('Escape')
  await ctxB.close()
  await page.setViewportSize({ width: 900, height: 900 })

  // The remembered choice is preselected on the next visit, never applied on its own.
  const before = await fitted()
  await page.reload()
  await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.waitForTimeout(1_000)
  expect(await fitted()).toEqual(before)
  await expect(control).not.toHaveText(/· fit/)

  // Phone width: the control fits beside the badges.
  await page.setViewportSize({ width: 390, height: 844 })
  await shot(page, 'terminal-size-390')
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow).toBeLessThanOrEqual(0)
  await page.setViewportSize({ width: 1280, height: 900 })

  // An operator watches and messages but cannot resize (the control is disabled, with the reason); a viewer gets no
  // control at all.
  const people = [
    { email: 'size-operator@example.com', role: 'operator', disabled: true },
    { email: 'size-viewer@example.com', role: 'viewer', disabled: false },
  ]
  for (const p of people) {
    await asRoot.call('POST', '/users', { display_name: p.role, email: p.email, password, grants: [{ role: p.role, domain: sp.id }] })
    const ctx = await browser.newContext({ baseURL })
    const other = await ctx.newPage()
    await new Api(other.request, base).signIn(p.email, password)
    await other.goto(runPath)
    if (p.disabled) {
      await expect(other.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
      // Focusable, with the reason as its description (keyboard and screen readers), and it opens nothing.
      const blocked = other.getByTestId('terminal-size')
      await expect(blocked).toHaveAttribute('aria-disabled', 'true')
      await expect(blocked).toHaveAccessibleDescription(/terminal takeover permission/)
      await blocked.focus()
      await expect(blocked).toBeFocused()
      await other.keyboard.press('Enter')
      await expect(other.getByTestId('terminal-size-fit')).toHaveCount(0)
    } else {
      // Viewers see the run but not its terminal stream (no terminal:watch), and no size control.
      await expect(other.getByTestId('run-header')).toBeVisible({ timeout: 20_000 })
      await expect(other.getByTestId('terminal-size')).toHaveCount(0)
    }
    await ctx.close()
  }

  // Back to the node's default: the hold closes and the program redraws at the start size.
  await control.click()
  await page.getByTestId('terminal-size-default').click()
  await expect(control).toHaveText(/Node default/)
  const [c, r] = start.split('x').map(Number)
  await expect.poll(() => drewAt(c, r), { timeout: 20_000 }).toBe(true)

  // The replay redraws at each recorded size: at the end it shows the start size again.
  await page.getByText('Replay', { exact: true }).click()
  await expect(page.getByTestId('replay-toolbar')).toBeVisible({ timeout: 10_000 })
  await expect(page.getByTestId('terminal-badge-size')).toHaveText(`${c}×${r}`, { timeout: 10_000 })
})
