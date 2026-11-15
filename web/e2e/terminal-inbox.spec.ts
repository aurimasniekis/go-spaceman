import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * "Send as inbox item" in a run's terminal view (operator request, 2026-11-13): an operator of the space, who may not
 * take over the terminal, turns the message box's text into an inbox item. The dialog opens prefilled with the text and
 * a reference to the run; once the item is added, a toast links to it and the message box is empty. Nothing reaches
 * the terminal: no input request leaves the page and the run's recording never contains the text.
 *
 * It creates its own users, so it must sort after smoke.spec.ts, which needs a stack that is not set up yet.
 */
test('an operator sends the terminal message box to the inbox', async ({ page }) => {
  test.setTimeout(120_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const id = Date.now().toString(36)

  const nodes = await asRoot.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await asRoot.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: `terminal-inbox-${id}` })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await asRoot.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await asRoot.call<{ id: string }>('POST', '/spaces', { slug: `terminal-inbox-${id}`, name: `Terminal inbox ${id}`, node_id: nodeId },
    { 'Idempotency-Key': `terminal-inbox-${id}` })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: `terminal-inbox-${id}`, kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await asRoot.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)
  const password = 'terminal inbox journey password'
  await asRoot.call('POST', '/users', { display_name: 'Terminal inbox operator', email: `terminal-inbox-${id}@example.com`, password, grants: [{ role: 'operator', domain: sp.id }] })

  const run = await asRoot.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'inboxer', harness: 'claude-code', prompt: 'hello' })
  const runPath = `/spaces/${sp.id}/runs/${run.id}`
  try {
    await expect.poll(async () => (await asRoot.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).toBe('running')

    await new Api(page.request, base).signIn(`terminal-inbox-${id}@example.com`, password)
    const inputs: string[] = []
    page.on('request', (r) => {
      if (r.method() === 'POST' && /\/runs\/[^/]+\/(input|takeover)/.test(new URL(r.url()).pathname)) inputs.push(r.url())
    })
    await page.goto(runPath)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
    // An operator cannot take over, and doesn't need to.
    await expect(page.getByTestId('run-header-actions').getByRole('button', { name: /Take over/ })).toHaveCount(0)
    await expect(page.getByTestId('run-header-actions').getByRole('button', { name: 'New inbox item' })).toBeVisible()

    const box = page.getByRole('textbox', { name: 'Message the agent' })
    const sendAs = page.getByRole('button', { name: 'Send as inbox item' })
    await expect(sendAs).toBeDisabled()

    // On a phone both are icons that fit beside the other actions, with no sideways scrolling.
    await page.setViewportSize({ width: 390, height: 844 })
    for (const b of [page.getByTestId('run-header-actions').getByRole('button', { name: 'New inbox item' }), sendAs, page.getByRole('button', { name: 'Send', exact: true })]) {
      const bb = (await b.boundingBox())!
      expect(bb.x + bb.width).toBeLessThanOrEqual(390)
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0)
    await page.setViewportSize({ width: 1280, height: 900 })
    const text = `Check the flaky upload test ${id}`
    await box.fill(text)
    await sendAs.click()

    const dialog = page.getByRole('dialog', { name: 'New inbox item' })
    const body = dialog.getByRole('textbox', { name: 'Item text' })
    await expect(body).toContainText(text)
    await expect(body).toContainText(`From the terminal of inboxer (run ${run.id})`)
    await dialog.getByRole('button', { name: 'Add to inbox' }).click()
    await expect(dialog).toBeHidden()

    const link = page.getByTestId('new-inbox-item-link')
    await expect(link).toHaveText(text)
    await expect(box).toHaveValue('')
    await expect(page).toHaveURL(new RegExp(`${runPath}$`))

    const items = await asRoot.call<{ items: { id: string; title: string }[] }>('GET', `/spaces/${sp.id}/inbox`)
    const item = items.items.find((i) => i.title === text)
    expect(item).toBeDefined()
    const full = await asRoot.call<{ body: string }>('GET', `/spaces/${sp.id}/inbox/${item!.id}`)
    expect(full.body).toContain(run.id)

    await link.click()
    await expect(page).toHaveURL(new RegExp(`/spaces/${sp.id}/inbox/${item!.id}$`))
    await page.goto(`/spaces/${sp.id}/inbox`)
    await expect(page.getByRole('link', { name: text })).toBeVisible()

    expect(inputs).toEqual([])
    const rec = await asRoot.call<{ records: { k: string; d?: string }[] }>('GET', `${runPath}/recording?limit=5000`)
    expect(rec.records.some((r) => Buffer.from(r.d ?? '', 'base64').toString('utf8').includes(text))).toBe(false)
  } finally {
    await asRoot.call('POST', `${runPath}/stop`, { mode: 'kill', reason: 'e2e cleanup' }).catch(() => {})
  }
})
