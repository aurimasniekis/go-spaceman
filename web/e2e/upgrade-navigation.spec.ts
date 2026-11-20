import { readFileSync } from 'node:fs'
import { join } from 'node:path'

import AxeBuilder from '@axe-core/playwright'
import { devices, expect, test, type Browser, type BrowserContextOptions, type Page } from '@playwright/test'

import { Api, env } from './stack'

/**
 * Client-side navigation between the top-level pages, on a desktop and at phone size, and after an upgrade while the
 * tab was open. Routes are split into chunks with hashed names; a tab opened before an upgrade asks for names the new
 * build no longer has. The server answers those with a 404 (as a real upgrade does) and the client must recover with
 * one reload instead of "Something went wrong!", and without showing any error screen while that reload is on its way.
 * The phone sizes run in Chromium with the device's viewport, touch and user agent (WebKit needs system libraries this
 * host may not have).
 *
 * What this does and does not cover: every browser here is Chromium, so the import fails with Chromium's wording
 * ("Failed to fetch dynamically imported module: <url>"), including in the "iPhone 13" project, which only borrows
 * Safari's user agent. TanStack Router reloads by itself for that wording, so on the build before this fix /spaces and
 * /terminals already recovered here; what failed was the stylesheet preload (/human, "Unable to preload CSS"), the
 * chunk that keeps failing, and the wrong error screen shown before the reload landed. Safari's wording ("Importing a
 * module script failed.", the same for every module) is covered by the unit tests in RouteError.test.tsx.
 *
 * It creates its own user, so it must sort after smoke.spec.ts, which needs a stack that is not set up yet.
 */

const emulate = ({ defaultBrowserType: _, ...d }: (typeof devices)[string]): BrowserContextOptions => d
const viewports: { name: string; mobile: boolean; options: BrowserContextOptions }[] = [
  { name: 'desktop', mobile: false, options: { viewport: { width: 1440, height: 900 } } },
  { name: 'Pixel 7', mobile: true, options: emulate(devices['Pixel 7']) },
  { name: 'iPhone 13', mobile: true, options: emulate(devices['iPhone 13']) },
]

// The admin sees these sidebar entries (web/src/lib/nav.ts): the plain ones, then the three settings hubs. Each hub opens on
// its first tab, and every page of a hub is a tab link under the page header.
const entries: [label: string | RegExp, path: string][] = [
  ['Overview', '/'], [/^Needs you/, '/human'], ['Terminals', '/terminals'], ['Spaces', '/spaces'],
]
const hubs: { label: string; entry: string; tabs: [label: string, path: string][] }[] = [
  { label: 'Configuration', entry: '/configuration', tabs: [
    ['Prompts', '/prompts'], ['Roles & profiles', '/roles'], ['Skills', '/skills'], ['Approval policies', '/approval-policies'],
    ['Action types', '/human-types'], ['Cross-space', '/cross-space'], ['Nodes', '/nodes'],
  ] },
  { label: 'Integrations', entry: '/integrations', tabs: [
    ['Events', '/events'], ['MCP servers', '/mcp-servers'], ['Accounts', '/accounts'], ['Git SSH keys', '/git-keys'], ['Harbor Pilot', '/harbor-pilot'],
  ] },
  { label: 'Access', entry: '/access', tabs: [
    ['Users', '/users'], ['Sign-in providers', '/sign-in-providers'], ['API tokens', '/tokens'], ['Audit log', '/audit'], ['Backups', '/backups'], ['System', '/system'],
  ] },
]
// Off the sidebar but still routes: Attention and Account (the user menu).
const offSidebar = ['/attention', '/account']

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

const errorScreen = /Something went wrong|This page failed to load|SpaceMan was updated/
const shownKey = 'e2e:error-screens'

/**
 * A signed-in page; uncaught page errors and full page loads are collected for the assertions, and so is every error
 * screen the page shows at any moment (in sessionStorage, so it survives the reload), even one replaced a moment later.
 */
async function open(browser: Browser, options: BrowserContextOptions) {
  const context = await browser.newContext({ ...options, storageState: state, colorScheme: 'dark' })
  await context.addInitScript(([key, source]) => {
    const pattern = new RegExp(source)
    new MutationObserver(() => {
      for (const el of document.querySelectorAll('[role="alert"]')) {
        const text = el.textContent ?? ''
        if (!pattern.test(text)) continue
        const shown: string[] = JSON.parse(sessionStorage.getItem(key) ?? '[]')
        if (!shown.includes(text)) sessionStorage.setItem(key, JSON.stringify([...shown, text]))
      }
    }).observe(document, { childList: true, subtree: true, characterData: true })
  }, [shownKey, errorScreen.source] as const)
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

/** Every error screen the page has shown so far, including on the page before a reload. */
const shownErrors = async (page: Page): Promise<string[]> => JSON.parse((await page.evaluate((k) => sessionStorage.getItem(k), shownKey)) ?? '[]')

async function expectPage(page: Page, path: string) {
  await expect(page).toHaveURL((u) => u.pathname === path)
  await expect(page.getByRole('main')).toContainText(/\w/)
  await expect(page.getByText(errorScreen)).toHaveCount(0)
  expect(await shownErrors(page), 'no error screen, not even before the reload').toEqual([])
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
    // "iPhone 13" differs from Pixel 7 here only in size and user agent (both are Chromium), so it skips the walk
    // through every page and keeps the upgrade cases.
    if (vp.name !== 'iPhone 13')
      test('every sidebar entry, hub and tab opens by client-side navigation', async ({ browser }) => {
        test.setTimeout(240_000)
        const { page, errors, reloads } = await open(browser, vp.options)
        const sidebar = page.getByRole('navigation', { name: 'Main navigation' })
        for (const [label, path] of entries) {
          await navigate(page, vp.mobile, label)
          await expectPage(page, path)
        }
        for (const hub of hubs) {
          // The sidebar has the hub, not its pages, and the hub opens on its first tab.
          await navigate(page, vp.mobile, hub.label)
          await expectPage(page, hub.tabs[0][1])
          const tabs = page.getByRole('main').getByRole('navigation', { name: `${hub.label} sections` })
          for (const [label, path] of hub.tabs) {
            const tab = tabs.getByRole('link', { name: label, exact: true })
            await tab.click()
            await expectPage(page, path)
            // The tab bar is part of the page header, so it is the one to carry the current page.
            await expect(tabs.getByRole('link', { name: label, exact: true })).toHaveAttribute('aria-current', 'page')
            await expect(tabs.locator('[aria-current="page"]')).toHaveCount(1)
          }
        }
        // The hubs are the only way into those pages, and nothing the sidebar used to list is there any more.
        if (!vp.mobile) {
          for (const gone of ['Attention', 'Account', 'Nodes', 'API tokens', 'Prompts']) await expect(sidebar.getByRole('link', { name: gone, exact: true })).toHaveCount(0)
        }
        expect(reloads, 'client-side navigation only').toEqual([])
        expect(errors).toEqual([])
        await page.context().close()
      })

    // Every old URL still loads its page directly (Go redirects, the Mini App, banners and bookmarks link to them), and the
    // hubs' entry routes land on the first tab the user may see.
    if (!vp.mobile)
      test('old URLs load directly and the entry routes redirect to the first tab', async ({ browser }) => {
        test.setTimeout(180_000)
        const { page, errors } = await open(browser, vp.options)
        for (const path of [...hubs.flatMap((h) => h.tabs.map(([, p]) => p)), ...offSidebar]) {
          await page.goto(path)
          await expectPage(page, path)
        }
        for (const hub of hubs) {
          await page.goto(hub.entry)
          await expectPage(page, hub.tabs[0][1])
        }
        expect(errors).toEqual([])
        await page.context().close()
      })

    // Spaces and Terminals are what the operator opened; Spaces is two lazy routes (spaces.tsx, spaces.index.tsx), so
    // it fails twice, once per chunk. Needs you's chunk also has its own stylesheet, whose failure ("Unable to preload
    // CSS") TanStack Router does not reload for.
    for (const [label, path] of [['Spaces', '/spaces'], ['Terminals', '/terminals'], [/^Needs you/, '/human']] as const) {
      test(`after an upgrade while the tab was open, ${path} recovers with one reload`, async ({ browser }) => {
        const { page, reloads } = await open(browser, vp.options)
        await upgradeWhileOpen(page)
        await navigate(page, vp.mobile, label)
        await expectPage(page, path)
        expect(reloads).toEqual([new URL(path, page.url()).href])
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
  // A loop would reload again after the message: the absence of a reload can only be seen by waiting a moment.
  await page.waitForTimeout(2_000)
  expect(reloads).toEqual([new URL('/human', page.url()).href])
  // Only the message after the reload: nothing on the page before it.
  expect((await shownErrors(page)).map((t) => t.match(errorScreen)?.[0])).toEqual(['SpaceMan was updated'])
  await page.context().close()
})

// Every hub page at phone and desktop width, in both color schemes, with axe (WCAG 2.1 A/AA), without sideways scrolling,
// and with the tab bar reachable by keyboard (tabs are links, so Tab reaches them in order).
for (const width of [390, 1280]) {
  for (const scheme of ['light', 'dark'] as const) {
    test(`hub pages at ${width}px in the ${scheme} scheme pass axe`, async ({ browser }) => {
      test.setTimeout(240_000)
      const context = await browser.newContext({ viewport: { width, height: width < 768 ? 844 : 900 }, storageState: state, colorScheme: scheme })
      // Mantine reads its scheme from localStorage (the app's default is dark), before the page renders.
      await context.addInitScript((s) => localStorage.setItem('mantine-color-scheme-value', s), scheme)
      const page = await context.newPage()
      for (const hub of hubs) {
        await page.goto(hub.entry)
        for (const [label, path] of hub.tabs) {
          const tab = page.getByRole('main').getByRole('navigation', { name: `${hub.label} sections` }).getByRole('link', { name: label, exact: true })
          await tab.scrollIntoViewIfNeeded()
          await tab.click()
          await expect(page).toHaveURL((u) => u.pathname === path)
          await expect(page.getByRole('main').getByRole('heading').first()).toBeVisible()
          const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
          const found = res.violations.map((v) => `${v.id} [${v.impact}]: ${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')}`)
          expect(found, `${hub.label} / ${label}: accessibility violations`).toEqual([])
          const w = await page.evaluate(() => ({ page: document.documentElement.scrollWidth, view: document.documentElement.clientWidth }))
          expect(w.page, `${hub.label} / ${label}: page scrolls sideways`).toBeLessThanOrEqual(w.view)
        }
      }
      await context.close()
    })
  }
}
