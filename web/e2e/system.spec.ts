import { expect, test, type Page } from '@playwright/test'
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { Api, env, settle } from './stack'

/**
 * The System page (SM-OBS-006) through the real UI: an administrator sees the instance's status, versions, activity
 * and where it runs at phone (390 px) and desktop (1280 px) widths in both themes without horizontal scrolling; the
 * version is in the account menu for everyone; a global operator has no System link and the page refuses them.
 * Screenshots go to SM_SHOTS (for review; the documentation shots come from screenshots.spec.ts).
 */

const shots = process.env.SM_SHOTS ?? join(process.cwd(), 'test-results', 'shots')
mkdirSync(shots, { recursive: true })

const THEMES = ['dark', 'light'] as const
const WIDTHS = [390, 1280] as const

async function setTheme(page: Page, theme: (typeof THEMES)[number]) {
  await page.evaluate((t) => localStorage.setItem('mantine-color-scheme-value', t), theme)
}

async function noSideways(page: Page, what: string) {
  const w = await page.evaluate(() => ({ page: document.documentElement.scrollWidth, view: document.documentElement.clientWidth }))
  expect(w.page, `${what}: page scrolls sideways`).toBeLessThanOrEqual(w.view)
}

test('System page: status for administrators at 390 and 1280 px, version in the account menu, hidden from operators', async ({ page }) => {
  test.setTimeout(120_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'system page journey password'
  await asRoot.call('POST', '/users', { display_name: 'Sam Admin', email: 'system-admin@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  await asRoot.call('POST', '/users', { display_name: 'Olu Operator', email: 'system-ops@example.com', password, grants: [{ role: 'operator', domain: '*' }] })
  const info = await asRoot.call<{ version: string }>('GET', '/system/info')
  const api = new Api(page.request, base)
  await api.signIn('system-admin@example.com', password)

  for (const width of WIDTHS) {
    await page.setViewportSize({ width, height: width < 768 ? 844 : 900 })
    for (const theme of THEMES) {
      await page.goto('/system')
      await setTheme(page, theme)
      await page.reload()
      const status = page.getByTestId('system-status')
      await expect(status.getByText('Accepting writes')).toBeVisible()
      await expect(status.getByText(info.version, { exact: true })).toBeVisible()
      await expect(status.getByText(/^boot_/)).toBeVisible()
      await expect(page.getByTestId('system-counts')).toContainText('Spaces')
      await expect(page.getByTestId('system-instance')).toContainText('Data directory')
      await settle(page)
      await noSideways(page, `system ${width}px ${theme}`)
      await page.screenshot({ path: join(shots, `system-${width}-${theme}.png`), fullPage: true, animations: 'disabled' })
    }
  }

  // The version in the account menu.
  await page.getByRole('button', { name: /account menu/ }).click()
  await expect(page.getByTestId('app-version')).toHaveText(`SpaceMan ${info.version}`)
  await page.keyboard.press('Escape')

  // A global operator: no System link, and the page says it is for administrators. (Same context: a second browser
  // context fails under SM_CHROMIUM_ARGS=--single-process.)
  await page.context().clearCookies()
  await new Api(page.request, base).signIn('system-ops@example.com', password)
  await page.goto('/')
  await expect(page.getByRole('link', { name: 'Nodes' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'System' })).toHaveCount(0)
  await page.goto('/system')
  await expect(page.getByText('The System page is for global administrators.')).toBeVisible()
  await expect(page.getByTestId('system-status')).toHaveCount(0)
  await page.getByRole('button', { name: /account menu/ }).click()
  await expect(page.getByTestId('app-version')).toHaveText(`SpaceMan ${info.version}`)
})
