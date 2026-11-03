import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * The live terminal of a full-screen program survives being left and reopened inside the app (no reload). With
 * ghostty-web 0.4.0, disposing a terminal that drew Claude Code's full-screen start-up made the next terminal's resize
 * hang the tab (Chrome: RESULT_CODE_HUNG); the view resets a terminal before disposing it. The stub harness's
 * "/fullscreen" line prints that start-up. The wheel also scrolls a terminal taller than its box.
 */
test('reopening a full-screen run terminal', async ({ page }) => {
  test.setTimeout(120_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal reopen journey password'
  await asRoot.call('POST', '/users', { display_name: 'Terminal Tester', email: 'terminal@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('terminal@example.com', password)

  // The stack's node when an earlier journey enrolled it, otherwise a new one.
  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'terminal-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'fullscreen', name: 'Full screen', node_id: nodeId }, { 'Idempotency-Key': 'terminal-reopen-space' })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: 'fullscreen-stub', kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)
  const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'fullscreen', harness: 'claude-code', prompt: '/fullscreen' })
  await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${run.id}`)).observed, { timeout: 30_000 }).toBe('running')

  // Open the page once the start-up is recorded, as when someone opens a run that is already running: the terminal
  // draws it from the stored recording.
  await expect.poll(async () => {
    const rec = await api.call<{ records: { k: string; d?: string }[] }>('GET', `/spaces/${sp.id}/runs/${run.id}/recording`)
    return rec.records.some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('latin1').includes('[?1049h'))
  }, { timeout: 20_000 }).toBe(true)
  await page.goto(`/spaces/${sp.id}/runs/${run.id}`)
  await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
  for (let i = 0; i < 3; i++) {
    await page.getByRole('link', { name: 'Full screen' }).first().click()
    await expect(page).toHaveURL(new RegExp(`/spaces/${sp.id}$`))
    await page.goBack()
    // A hung tab never shows the badge again, and never answers.
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 15_000 })
    expect(await Promise.race([page.evaluate(() => 'responds'), new Promise((r) => setTimeout(() => r('hung'), 5_000))])).toBe('responds')
  }

  // When the terminal is taller than its box, the wheel scrolls the box (ghostty-web cancels wheel events itself).
  const box = page.getByTestId('terminal').locator('..')
  const before = await box.evaluate((el) => {
    el.scrollTop = 0
    return el.scrollHeight - el.clientHeight
  })
  if (before > 0) {
    const bb = await box.boundingBox()
    await page.mouse.move(bb!.x + bb!.width / 2, bb!.y + 40) // over the terminal, inside the box's visible part
    await page.mouse.wheel(0, 200)
    await expect.poll(async () => box.evaluate((el) => el.scrollTop), { timeout: 5_000 }).toBeGreaterThan(0)
  }
})
