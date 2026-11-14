import { expect, test } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env } from './stack'

/**
 * "New inbox item" on a space's overview (operator request, 2026-11-13): an operator of the space opens the same dialog
 * as the Inbox page from the space header, adds an item with ⌘/Ctrl+Enter, gets a confirmation that links to the item
 * and finds it in the inbox. A viewer of the space is not offered the action.
 *
 * It creates its own users, so it must sort after smoke.spec.ts, which needs a stack that is not set up yet.
 */

test('an operator adds an inbox item from the space overview', async ({ page, browser }) => {
  test.setTimeout(90_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const id = Date.now().toString(36)
  const nodes = await asRoot.call<{ items: { id: string }[] }>('GET', '/nodes')
  const nodeId = nodes.items[0]?.id ?? (await asRoot.call<{ node: { id: string } }>('POST', '/nodes', { name: `overview-inbox-${id}` })).node.id
  const sp = await asRoot.call<{ id: string }>('POST', '/spaces', { slug: `overview-inbox-${id}`, name: `Overview inbox ${id}`, node_id: nodeId },
    { 'Idempotency-Key': `overview-inbox-${id}` })
  const password = 'overview inbox journey password'
  for (const role of ['operator', 'viewer'])
    await asRoot.call('POST', '/users', { display_name: `Overview ${role}`, email: `overview-${role}-${id}@example.com`, password, grants: [{ role, domain: sp.id }] })

  // A viewer sees the overview without the action.
  const viewer = await browser.newContext({ baseURL: base })
  await new Api(viewer.request, base).signIn(`overview-viewer-${id}@example.com`, password)
  const vp = await viewer.newPage()
  await vp.goto(`/spaces/${sp.id}`)
  await expect(vp.getByTestId('space-header').getByText(`Overview inbox ${id}`)).toBeVisible()
  await expect(vp.getByRole('heading', { name: 'Agents' })).toBeVisible()
  await expect(vp.getByRole('button', { name: 'New inbox item' })).toHaveCount(0)
  await viewer.close()

  await new Api(page.request, base).signIn(`overview-operator-${id}@example.com`, password)
  await page.goto(`/spaces/${sp.id}`)
  const header = page.getByTestId('space-header')
  await header.getByRole('button', { name: 'New inbox item' }).click()
  const dialog = page.getByRole('dialog', { name: 'New inbox item' })
  const body = dialog.getByRole('textbox', { name: 'Item text' })
  await expect(body).toBeFocused()
  const title = `Rotate the staging certificates ${id}`
  await body.pressSequentially(`${title}\n`)
  // Closing the dialog keeps the unsent text.
  await page.keyboard.press('Escape')
  await expect(dialog).toBeHidden()
  await header.getByRole('button', { name: 'New inbox item' }).click()
  await expect(body).toContainText(title)
  await body.press('ControlOrMeta+Enter')
  await expect(dialog).toBeHidden()

  // Confirmed with a link, and the user stays on the overview.
  const link = page.getByTestId('new-inbox-item-link')
  await expect(link).toHaveText(title)
  await expect(page).toHaveURL(new RegExp(`/spaces/${sp.id}$`))

  await link.click()
  await expect(page).toHaveURL(new RegExp(`/spaces/${sp.id}/inbox/inb_`))
  await expect(page.getByText(title).first()).toBeVisible()
  await page.goto(`/spaces/${sp.id}/inbox`)
  await expect(page.getByRole('link', { name: title })).toBeVisible()
})
