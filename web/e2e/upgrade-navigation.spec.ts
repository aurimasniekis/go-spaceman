import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import { devices, expect, test, type Browser, type BrowserContextOptions, type Page } from '@playwright/test'

import { Api, env } from './stack'

/**
 * Client-side navigation between the top-level pages, on a desktop and at phone size, and after an upgrade while the
 * tab was open. Routes are split into chunks with hashed names; a tab opened before an upgrade asks for names the new
 * build no longer has. The server answers those with a 404 (as a real upgrade does) and the client must recover with
 * one reload instead of "Something went wrong!". The phone sizes run in Chromium with the device's viewport, touch and
 * user agent (WebKit needs system libraries this host may not have).
 *
 * It creates its own user, so it must sort after smoke.spec.ts, which needs a stack that is not set up yet.
 */

const emulate = ({ defaultBrowserType: _, ...d }: (typeof devices)[string]): BrowserContextOptions => d
const viewports: { name: string; mobile: boolean; options: BrowserContextOptions }[] = [
  { name: 'desktop', mobile: false, options: { viewport: { width: 1440, height: 900 } } },
  { name: 'Pixel 7', mobile: true, options: emulate(devices['Pixel 7']) },
  { name: 'iPhone 13', mobile: true, options: emulate(devices['iPhone 13']) },
]

// The admin sees every entry of the main navigation (web/src/lib/nav.ts).
const pages: [label: string | RegExp, path: string][] = [
  ['Overview', '/'], ['Attention', '/attention'], [/^Needs you/, '/human'], ['Terminals', '/terminals'], ['Spaces', '/spaces'],
  ['Nodes', '/nodes'], ['Prompts', '/prompts'], ['Roles & profiles', '/roles'], ['Skills', '/skills'],
  ['Approval policies', '/approval-policies'], ['Action types', '/human-types'], ['Cross-space', '/cross-space'],
  ['Events', '/events'], ['MCP servers', '/mcp-servers'], ['Accounts', '/accounts'], ['Git SSH keys', '/git-keys'],
  ['Harbor Pilot', '/harbor-pilot'], ['Users', '/users'], ['Sign-in providers', '/sign-in-providers'], ['API tokens', '/tokens'],
  ['Audit log', '/audit'], ['Backups', '/backups'], ['System', '/system'], ['Account', '/account'],
]

let state = ''
test.beforeAll(async ({ playwright }) => {
  test.setTimeout(90_000)
  const base = env('SM_URL')
  const req = await playwright.request.newContext({ baseURL: base })
  const asRoot = new Api(req, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const email = `navigation-${Date.now()}@example.com`
  const password = 'navigation journey password'
  await asRoot.call('POST', '/users', { display_name: 'Navigation Tester', email, password, grants: [{ role: 'admin', domain: '*' }] })
  await new Api(req, base).signIn(email, password)
  state = join(env('SM_DIR'), 'navigation-state.json')
  await req.storageState({ path: state })
  await req.dispose()
})

/** A signed-in page; uncaught page errors and the router's error screens are collected for the assertions. */
async function open(browser: Browser, options: BrowserContextOptions) {
  const context = await browser.newContext({ ...options, storageState: state, colorScheme: 'dark' })
  const page = await context.newPage()
  const errors: string[] = []
  page.on('pageerror', (e) => errors.push(e.message))
  const reloads: string[] = []
  page.on('request', (r) => {
    if (r.isNavigationRequest() && r.frame() === page.mainFrame()) reloads.push(r.url())
  })
  await page.goto('/')
  await expect(page.getByRole('main').getByRole('heading', { name: 'Overview' })).toBeVisible()
  reloads.length = 0
  return { page, errors, reloads }
}

async function navigate(page: Page, mobile: boolean, label: string | RegExp) {
  const link = page.getByRole('navigation', { name: 'Main navigation' }).getByRole('link', { name: label, exact: true })
  // At phone size the navigation slides in from the left: closed, it is still "visible" but off-screen. Wait for it
  // to finish sliding (it closes after each navigation), then open it.
  const drawer = async () =>
    link.evaluate((el) => {
      const r = el.getBoundingClientRect()
      return r.left >= 0 ? 'open' : r.right <= 0 ? 'closed' : 'moving'
    })
  if (mobile) {
    await expect.poll(drawer).not.toBe('moving')
    if ((await drawer()) === 'closed') {
      await page.getByRole('button', { name: 'Toggle navigation' }).click()
      await expect.poll(drawer).toBe('open')
    }
  }
  await link.click()
}

async function expectPage(page: Page, path: string) {
  await expect(page).toHaveURL((u) => u.pathname === path)
  await expect(page.getByRole('main')).toContainText(/\w/)
  await expect(page.getByText(/Something went wrong|This page failed to load|SpaceMan was updated/)).toHaveCount(0)
}

/**
 * An upgrade while the tab is open: from now on, every asset this page has not loaded yet is answered with the
 * server's real response for a name it does not have (a 404), until the page loads index.html again, which then names
 * the current build's chunks. With `forever`, the matching assets keep failing after a reload as well.
 */
async function upgradeWhileOpen(page: Page, forever?: RegExp) {
  const loaded = new Set<string>()
  let stale = true
  page.on('request', (r) => {
    if (r.isNavigationRequest() && r.frame() === page.mainFrame()) stale = false
  })
  // Recorded before the upgrade, so these stay served from the old page's module map.
  for (const e of await page.evaluate(() => performance.getEntriesByType('resource').map((r) => r.name))) loaded.add(new URL(e).pathname)
  await page.route('**/assets/**', async (route) => {
    const name = new URL(route.request().url()).pathname
    if ((stale && !loaded.has(name)) || forever?.test(name)) {
      await route.fulfill({ response: await page.request.get(`/assets/upgraded-away-${name.split('/').pop()}`) })
      return
    }
    await route.continue()
  })
}

for (const vp of viewports) {
  test.describe(vp.name, () => {
    test('every top-level page opens from the navigation', async ({ browser }) => {
      test.setTimeout(180_000)
      const { page, errors, reloads } = await open(browser, vp.options)
      for (const [label, path] of pages) {
        await navigate(page, vp.mobile, label)
        await expectPage(page, path)
      }
      expect(reloads, 'client-side navigation only').toEqual([])
      expect(errors).toEqual([])
      await page.context().close()
    })

    // Spaces and Terminals are what the operator opened; Needs you's chunk also has its own stylesheet, whose failure
    // ("Unable to preload CSS") TanStack Router does not reload for.
    for (const [label, path] of [['Spaces', '/spaces'], ['Terminals', '/terminals'], [/^Needs you/, '/human']] as const) {
      test(`after an upgrade while the tab was open, ${path} recovers with one reload`, async ({ browser }) => {
        const { page, reloads } = await open(browser, vp.options)
        await upgradeWhileOpen(page)
        await navigate(page, vp.mobile, label)
        await expectPage(page, path)
        expect(reloads.length, `reloads: ${reloads.join(', ')}`).toBeGreaterThanOrEqual(1)
        expect(reloads.length, `reloads: ${reloads.join(', ')}`).toBeLessThanOrEqual(2)
        await page.context().close()
      })
    }
  })
}

test('a chunk that keeps failing shows "SpaceMan was updated" with Reload, without a reload loop', async ({ browser }) => {
  const { page, reloads } = await open(browser, viewports[1].options)
  await upgradeWhileOpen(page, /\/assets\/(human-|composer-)/)
  await navigate(page, true, /^Needs you/)
  const alert = page.getByRole('alert').filter({ hasText: 'SpaceMan was updated' })
  await expect(alert).toBeVisible()
  await expect(alert.getByRole('button', { name: 'Reload' })).toBeVisible()
  await page.waitForTimeout(2_000)
  expect(reloads.length, `reloads: ${reloads.join(', ')}`).toBeLessThanOrEqual(2)
  await page.context().close()
})
