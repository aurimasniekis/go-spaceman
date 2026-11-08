import { expect, test, type Page } from '@playwright/test'
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { Api, env, settle } from './stack'

/**
 * "Add SpaceMan MCP" (ADR-0063, AM-7) through the real UI: the modal's defaults (slug spaceman-admin, the control role,
 * "Administer everything (infra)", dangerous approvals locked on, no expiry) and live tool count, the created server's
 * badge and drawer, the internal token's label on the Tokens page, the space's MCP card, and a confirmed delete. The
 * modal and the pages are checked at phone (390 px) and desktop (1280 px) widths in both themes, without horizontal
 * scrolling. Screenshots go to SM_SHOTS (for review, not committed here).
 */

const shots = process.env.SM_SHOTS ?? join(process.cwd(), 'test-results', 'shots')
mkdirSync(shots, { recursive: true })

const THEMES = ['dark', 'light'] as const
const WIDTHS = [390, 1280] as const

async function setTheme(page: Page, theme: (typeof THEMES)[number]) {
  await page.evaluate((t) => localStorage.setItem('mantine-color-scheme-value', t), theme)
}

/** Fails when the page or the open dialog scrolls sideways. */
async function noSideways(page: Page, what: string) {
  const widths = await page.evaluate(() => {
    const doc = document.documentElement
    const dialog = document.querySelector<HTMLElement>('[role="dialog"] .mantine-Modal-body, [role="dialog"] .mantine-Drawer-body')
    return { page: doc.scrollWidth, view: doc.clientWidth, dialog: dialog ? dialog.scrollWidth - dialog.clientWidth : 0 }
  })
  expect(widths.page, `${what}: page scrolls sideways`).toBeLessThanOrEqual(widths.view)
  expect(widths.dialog, `${what}: dialog scrolls sideways`).toBeLessThanOrEqual(1)
}

test('Add SpaceMan MCP: modal, drawer, tokens and space card at 390 and 1280 px', async ({ page }) => {
  test.setTimeout(180_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'spaceman mcp journey password'
  await asRoot.call('POST', '/users', { display_name: 'Morgan Admin', email: 'mcp-admin@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('mcp-admin@example.com', password)
  const node = await api.call<{ node: { id: string } }>('POST', '/nodes', { name: 'mcp-journey-node' }, { 'Idempotency-Key': 'spaceman-mcp-node' })
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'infra-mcp', name: 'Infra', node_id: node.node.id }, { 'Idempotency-Key': 'spaceman-mcp-space' })

  // The modal: defaults and the live tool count, at both widths and in both themes.
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
    for (const theme of THEMES) {
      await page.goto('/mcp-servers')
      await setTheme(page, theme)
      await page.reload()
      await page.getByRole('button', { name: 'Add SpaceMan MCP' }).click()
      const dialog = page.getByRole('dialog', { name: 'Add SpaceMan MCP' })
      await expect(dialog.getByLabel('Slug')).toHaveValue('spaceman-admin')
      await expect(dialog.getByRole('radio', { name: /Administer everything \(infra\)/ })).toBeChecked()
      await expect(dialog.getByRole('checkbox', { name: /Dangerous tools: always/ })).toBeDisabled()
      await expect(dialog.getByRole('switch', { name: /Writes need approval/ })).not.toBeChecked()
      await expect(dialog.getByRole('checkbox', { name: 'Never expires' })).toBeChecked()
      await expect(dialog.getByTestId('tool-count')).toHaveText(/^[1-9]\d* tools$/)
      await settle(page)
      await noSideways(page, `modal ${width}px ${theme}`)
      await page.screenshot({ path: join(shots, `spaceman-mcp-add-${width}-${theme}.png`), animations: 'disabled' })
      // The custom picker: tools grouped by tag, dangerous ones flagged.
      await dialog.getByText('Custom', { exact: true }).click()
      await expect(dialog.getByTestId('tool-count')).toHaveText('0 tools')
      await dialog.getByRole('button', { name: /^spaces \d/ }).click()
      await dialog.getByRole('button', { name: /^accounts \d/ }).click()
      await expect(dialog.getByText('dangerous · approval').filter({ visible: true }).first()).toBeVisible()
      await dialog.getByRole('checkbox', { name: 'get_space', exact: true }).check()
      await expect(dialog.getByTestId('tool-count')).toHaveText('1 tool')
      await settle(page)
      await noSideways(page, `custom tools ${width}px ${theme}`)
      await page.screenshot({ path: join(shots, `spaceman-mcp-custom-${width}-${theme}.png`), animations: 'disabled' })
      await page.keyboard.press('Escape')
    }
  }

  // Create it from the space's MCP card, with the space prefilled and the default preset.
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto(`/spaces/${sp.id}`)
  await setTheme(page, 'dark')
  await page.reload()
  await page.getByRole('button', { name: 'Add SpaceMan MCP' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add SpaceMan MCP' })
  await expect(dialog.getByRole('textbox', { name: 'Space', exact: true })).toHaveValue('Infra')
  await expect(dialog.getByLabel('Space role')).toHaveValue('control')
  await expect(dialog.getByTestId('tool-count')).toHaveText(/^[1-9]\d* tools$/)
  await dialog.getByRole('button', { name: 'Add server' }).click()
  await expect(dialog).toBeHidden()
  const card = page.locator('.mantine-Card-root', { hasText: 'MCP servers' })
  await expect(card.getByText('SpaceMan', { exact: true })).toHaveCount(2) // the server's name and its badge
  await expect(card.getByText('control')).toBeVisible()

  // The list badge and the drawer.
  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
    for (const theme of THEMES) {
      await page.goto('/mcp-servers')
      await setTheme(page, theme)
      await page.reload()
      const row = page.getByRole('row', { name: /SpaceMan/ })
      await expect(row.getByText('SpaceMan', { exact: true }).nth(1)).toBeVisible()
      await noSideways(page, `list ${width}px ${theme}`)
      await page.screenshot({ path: join(shots, `spaceman-mcp-list-${width}-${theme}.png`), animations: 'disabled' })
      await row.click()
      const drawer = page.getByRole('dialog', { name: 'SpaceMan' })
      await expect(drawer.getByText('Morgan Admin (mcp-admin@example.com)')).toBeVisible()
      await expect(drawer.getByRole('link', { name: /^atk_/ })).toBeVisible()
      await expect(drawer.getByTestId('effective-count')).toHaveText(/^[1-9]\d* tools$/)
      await expect(drawer.getByText(/· approval$/).first()).toBeVisible()
      await settle(page)
      await noSideways(page, `drawer ${width}px ${theme}`)
      await page.screenshot({ path: join(shots, `spaceman-mcp-drawer-${width}-${theme}.png`), animations: 'disabled' })
      await page.keyboard.press('Escape')
    }
  }

  // The Tokens page labels the internal token and has nothing to copy.
  await page.setViewportSize({ width: 1280, height: 900 })
  await page.goto('/tokens')
  const tokenRow = page.getByRole('row', { name: /SpaceMan MCP · internal/ })
  await expect(tokenRow).toBeVisible()
  await expect(tokenRow.getByText(/can’t be copied/)).toBeVisible()
  await expect(tokenRow.getByRole('button', { name: /copy/i })).toHaveCount(0)
  await page.screenshot({ path: join(shots, 'spaceman-mcp-tokens-1280.png'), animations: 'disabled' })

  // Delete, confirmed: the token is revoked with it.
  await page.goto('/mcp-servers')
  await page.getByRole('row', { name: /SpaceMan/ }).click()
  await page.getByRole('button', { name: 'Delete…' }).click()
  await page.getByRole('dialog', { name: 'Delete this SpaceMan MCP server?' }).getByRole('button', { name: 'Delete' }).click()
  await expect(page.getByRole('row', { name: /SpaceMan/ })).toHaveCount(0)
  const tokens = await api.call<{ items: { purpose?: string; revoked_at?: string }[] }>('GET', '/tokens')
  expect(tokens.items.filter((t) => t.purpose).every((t) => t.revoked_at)).toBe(true)
})
