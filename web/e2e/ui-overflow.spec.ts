import { devices, expect, test, type Browser, type Page } from '@playwright/test'

import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { overflowProblems, phoneProblems, pinchCancelled, realScrollbars, smallTouchText, zoomBlockers, seedWorld, stopWorld, type World } from './overflow'
import { env, settle } from './stack'

/**
 * No unwanted scrollbars (operator report: the "New inbox item" dialog showed both scrollbars although its content fit,
 * and the space sections row showed a vertical one). Every page, modal and drawer is opened at phone (390 px), desktop
 * (1280 px) and wide (1920 px) widths, in both themes, with real scrollbars (Playwright's Chromium hides them by default,
 * which hides this class of bug), and checked by `overflowProblems`: no page or dialog that scrolls sideways, no modal that
 * scrolls although its content fits, no scroll container that overflows by a sliver. A long form still scrolls.
 * The file name sorts it after smoke.spec.ts, whose first-run journey needs a stack nobody has set up yet.
 */

test.use({ launchOptions: realScrollbars })

const WIDTHS = [390, 1280, 1920] as const
const height = (w: number) => (w < 768 ? 844 : 900)

type Step = (page: Page) => Promise<unknown>
interface Dialog {
  name: string
  path: (w: World) => string
  open: Step[]
  /** Only at these widths (a control that exists only on phones or only on wide screens). */
  widths?: readonly number[]
}

const T = { timeout: 5_000 }
const main = (page: Page) => page.locator('main')
/** Clicks a button by its name; on a phone, a page action that moved into the page's "More actions" menu is chosen there. */
const button = (name: string | RegExp): Step => async (page) => {
  const b = main(page).getByRole('button', { name, exact: typeof name === 'string' }).first()
  if (await b.waitFor({ state: 'visible', timeout: 3_000 }).then(() => true, () => false)) return b.click(T)
  await main(page).getByRole('button', { name: /^More actions/ }).first().click(T)
  return page.getByRole('menuitem', { name }).first().click(T)
}
const menu = (target: string | RegExp, item: string | RegExp): Step => async (page) => {
  await main(page).getByRole('button', { name: target }).first().click(T)
  await page.getByRole('menuitem', { name: item }).first().click(T)
}
const tab = (name: string): Step => (page) => main(page).getByRole('tab', { name }).first().click(T)
const text = (t: string | RegExp): Step => (page) => main(page).getByText(t).filter({ visible: true }).first().click(T)
const inDialog = (name: string | RegExp): Step => (page) => page.getByRole('dialog').last().getByRole('button', { name }).first().click(T)

const space = (w: World) => `/spaces/${w.space}`

const DIALOGS: Dialog[] = [
  // Spaces and nodes.
  { name: 'New space', path: () => '/spaces', open: [button('New space')] },
  { name: 'Archive a space', path: () => '/spaces', open: [menu(/^More actions for Docs site/, 'Archive…')] },
  { name: 'Purge a space', path: () => '/spaces', open: [(page) => page.getByText(/^Archived \(/).click(T), menu(/^More actions for Old reports/, 'Purge…')] },
  { name: 'Add node', path: () => '/nodes', open: [button('Add node')] },
  { name: 'Enrollment token', path: () => '/nodes', open: [menu(/^Actions for overflow-spare/, 'New enrollment token')] },
  // Catalog.
  { name: 'New prompt', path: () => '/prompts', open: [button('New prompt')] },
  { name: 'Prompt version', path: (w) => `/prompts/${w.prompt}`, open: [button('View')] },
  { name: 'Compare versions', path: (w) => `/prompts/${w.prompt}`, open: [button('Diff')] },
  { name: 'Fork version', path: (w) => `/prompts/${w.prompt}`, open: [button('Fork')] },
  { name: 'New role template', path: () => '/roles', open: [button('New role template')] },
  { name: 'Edit role template', path: () => '/roles', open: [text(/^Control$/)] },
  { name: 'New harness profile', path: () => '/roles', open: [tab('Harness profiles'), button('New profile')] },
  { name: 'Edit harness profile', path: () => '/roles', open: [tab('Harness profiles'), text('Claude Code (default model)')] },
  { name: 'New skill', path: () => '/skills', open: [button('New skill')] },
  { name: 'Skill drawer', path: () => '/skills', open: [text('HallPass gateway')] },
  { name: 'Add approval policy', path: () => '/approval-policies', open: [button('Add policy')] },
  { name: 'New human action type', path: () => '/human-types', open: [button('New type')] },
  { name: 'Human action type drawer', path: () => '/human-types', open: [text('Deploy window')] },
  { name: 'Default human action policy', path: () => '/human-types', open: [button('Edit')] },
  // Integrations.
  { name: 'New event template', path: () => '/events', open: [button('New template')] },
  { name: 'Add Pulse connection', path: () => '/events', open: [tab('Connections'), button('Add connection')] },
  { name: 'Edit Pulse connection', path: () => '/events', open: [tab('Connections'), button('Edit')] },
  { name: 'Edit template draft', path: (w) => `/events/templates/${w.template}`, open: [button('New draft')] },
  { name: 'Add SpaceMan MCP', path: () => '/mcp-servers', open: [button('Add SpaceMan MCP')] },
  { name: 'Add MCP server', path: () => '/mcp-servers', open: [button('Add server')] },
  { name: 'MCP server drawer', path: () => '/mcp-servers', open: [text('Ticket tracker')] },
  { name: 'SpaceMan MCP drawer', path: () => '/mcp-servers', open: [text('SpaceMan admin')] },
  { name: 'Re-issue SpaceMan MCP', path: () => '/mcp-servers', open: [text('SpaceMan admin'), inDialog('Re-issue…')] },
  { name: 'Add account', path: () => '/accounts', open: [button('Add account')] },
  { name: 'Edit account', path: () => '/accounts', open: [menu(/^Actions for overflow-stub/, 'Edit or rotate…')] },
  { name: 'Delete account', path: () => '/accounts', open: [menu(/^Actions for overflow-stub/, 'Delete…')] },
  { name: 'Generate Git SSH key', path: () => '/git-keys', open: [button('Generate key')] },
  { name: 'Git SSH key drawer', path: () => '/git-keys', open: [text(/^overflow-bot/)] },
  { name: 'Connect Harbor Pilot', path: () => '/harbor-pilot', open: [button('Connect')] },
  // Access.
  { name: 'New user', path: () => '/users', open: [button('New user')] },
  { name: 'User profile', path: () => '/users', open: [menu(/^Actions for Oli Overflow/, 'Edit profile')] },
  { name: 'User grants', path: () => '/users', open: [menu(/^Actions for Oli Overflow/, 'Edit grants')] },
  { name: 'User sign-in identities', path: () => '/users', open: [menu(/^Actions for Oli Overflow/, 'Sign-in identities…')] },
  { name: 'Password reset token', path: () => '/users', open: [menu(/^Actions for Oli Overflow/, 'Password reset token')] },
  { name: 'Add sign-in provider', path: () => '/sign-in-providers', open: [button('Add provider')] },
  { name: 'New API token', path: () => '/tokens', open: [button('New token')] },
  { name: 'Retention pass', path: () => '/backups', open: [button('Run a pass now')] },
  { name: 'Cross-space request drawer (all spaces)', path: () => '/cross-space', open: [text(/Which retry budget/)] },
  { name: 'Human action drawer (all spaces)', path: () => '/human', open: [text(/Should retries span/)] },
  { name: 'Operator message drawer (all spaces)', path: () => '/human?tab=messages', open: [text(/Guides are in step with the UI/)] },
  // A space.
  { name: 'Launch run', path: space, open: [button('Launch run')] },
  { name: 'Edit space', path: space, open: [menu(/^More actions for Payments/, 'Edit…')] },
  { name: 'Export space', path: space, open: [menu(/^More actions for Payments/, 'Export…')] },
  { name: 'Relocate space', path: space, open: [menu(/^More actions for Payments/, 'Relocate…')] },
  { name: 'Archive this space', path: space, open: [menu(/^More actions for Payments/, 'Archive…')] },
  { name: 'Add space role', path: space, open: [button('Add role')] },
  { name: 'Space role editor', path: space, open: [button('Edit')] },
  { name: 'Effective role configuration', path: space, open: [button('Effective configuration')] },
  { name: 'Space accounts', path: space, open: [button('Choose accounts')] },
  { name: 'Space Harbor Pilot', path: space, open: [(page) => page.getByTestId('space-harbor-pilot').getByRole('button', { name: 'Settings' }).click(T)] },
  { name: 'New inbox item', path: (w) => `${space(w)}/inbox`, open: [button('New item')] },
  { name: 'New inbox item from the overview', path: space, open: [button('New inbox item')] },
  { name: 'Create task from item', path: (w) => `${space(w)}/inbox/${w.item}`, open: [button('Create task')] },
  { name: 'Defer item', path: (w) => `${space(w)}/inbox/${w.item}`, open: [menu(/^More$/, 'Defer…')] },
  { name: 'Reject item', path: (w) => `${space(w)}/inbox/${w.item}`, open: [menu(/^More$/, 'Reject…')] },
  { name: 'New task', path: (w) => `${space(w)}/queue`, open: [button('New task')] },
  { name: 'Queue policy', path: (w) => `${space(w)}/queue`, open: [button('Policy')] },
  { name: 'Task drawer', path: (w) => `${space(w)}/queue`, open: [text(/^Add a retry budget per settlement batch/)] },
  { name: 'New human action', path: (w) => `${space(w)}/human`, open: [button('New action')] },
  { name: 'Human action policies', path: (w) => `${space(w)}/human`, open: [button('Policies')] },
  { name: 'Human action types in a space', path: (w) => `${space(w)}/human`, open: [button('Types')] },
  { name: 'Human action permissions', path: (w) => `${space(w)}/human`, open: [button('Permissions')] },
  { name: 'Edit a permission', path: (w) => `${space(w)}/human`, open: [button('Permissions'), inDialog(/^Edit the permission of/)] },
  { name: 'Human action drawer', path: (w) => `${space(w)}/human`, open: [text(/Should retries span/)] },
  { name: 'Withdraw a human action', path: (w) => `${space(w)}/human`, open: [text(/Should retries span/), inDialog(/^Withdraw/)] },
  { name: 'Send a message', path: (w) => `${space(w)}/messages`, open: [button('Send message')] },
  { name: 'Message drawer', path: (w) => `${space(w)}/messages`, open: [text(/Hold deploys until the bank window/)] },
  { name: 'Take over a terminal', path: (w) => `${space(w)}/terminals`, open: [button('Take over')] },
  { name: 'New inbox item from a terminal', path: (w) => `${space(w)}/terminals`, open: [button('New inbox item')] },
  { name: 'Terminals drawer', path: (w) => `/terminals?space=${w.space}`, open: [button(/^Space: /)], widths: [390] },
  { name: 'Add repository', path: (w) => `${space(w)}/repositories`, open: [button('Add repository')] },
  { name: 'Repository drawer', path: (w) => `${space(w)}/repositories`, open: [text(/^settlement$/)] },
  { name: 'Add knowledge base', path: (w) => `${space(w)}/knowledge`, open: [button('Add knowledge base')] },
  { name: 'Knowledge base drawer', path: (w) => `${space(w)}/knowledge`, open: [text('Payments knowledge')] },
  { name: 'Edit knowledge base', path: (w) => `${space(w)}/knowledge`, open: [text('Payments knowledge'), inDialog('Edit…')] },
  { name: 'Memory drawer', path: (w) => `${space(w)}/memory`, open: [text(/^control$/)] },
  { name: 'Add a dream', path: (w) => `${space(w)}/memory`, open: [button('Add dream')] },
  { name: 'Dreams drawer', path: (w) => `${space(w)}/memory`, open: [button('Nightly consolidation')] },
  { name: 'Subscribe to events', path: (w) => `${space(w)}/events`, open: [button('Subscribe')] },
  { name: 'Replay a subscription', path: (w) => `${space(w)}/events/${w.subscription}`, open: [menu('Cursor', 'Replay…')] },
  { name: 'New schedule', path: (w) => `${space(w)}/schedules`, open: [button('New schedule')] },
  { name: 'Schedule drawer', path: (w) => `${space(w)}/schedules`, open: [text('weekday review')] },
  { name: 'Edit schedule', path: (w) => `${space(w)}/schedules`, open: [text('weekday review'), inDialog(/^Edit/)] },
  { name: 'Edit space limits', path: (w) => `${space(w)}/limits`, open: [button('Edit limits')] },
  { name: 'Cross-space request drawer', path: (w) => `${space(w)}/cross-space`, open: [text(/Which retry budget/)] },
  { name: 'Add a peer space', path: (w) => `${space(w)}/cross-space`, open: [tab('Cross-space access'), button('Add peer')] },
]

const PAGES = (w: World): string[] => [
  '/', '/attention', '/human', '/human?tab=messages', '/terminals', '/spaces', '/usage', '/usage?group=model&range=90d', '/nodes', '/prompts', `/prompts/${w.prompt}`, '/roles', '/skills', '/approval-policies', '/human-types',
  '/cross-space', '/events', `/events/templates/${w.template}`, '/mcp-servers', '/accounts', '/git-keys', '/harbor-pilot', '/users', '/sign-in-providers',
  '/tokens', '/audit', '/backups', '/system', '/account',
  space(w), `${space(w)}/terminals`, `${space(w)}/inbox`, `${space(w)}/inbox/${w.item}`, `${space(w)}/queue`, `${space(w)}/human`, `${space(w)}/messages`,
  `${space(w)}/runs/${w.run}`, `${space(w)}/runs/${w.run}?view=replay`, `${space(w)}/runs/${w.live}`, `${space(w)}/repositories`, `${space(w)}/knowledge`,
  `${space(w)}/memory`, `${space(w)}/events`, `${space(w)}/events/${w.subscription}`, `${space(w)}/schedules`, `${space(w)}/limits`, `${space(w)}/cross-space`,
]

/** Checks the page as shown in both themes; the theme is switched in place (Mantine keys its colours on this attribute). */
async function check(page: Page, what: string, failures: string[]) {
  for (const theme of ['dark', 'light'] as const) {
    await page.evaluate((t) => document.documentElement.setAttribute('data-mantine-color-scheme', t), theme)
    for (const p of await overflowProblems(page)) failures.push(`${what} (${theme}): ${p.what}: ${p.el} (${p.scroll})`)
  }
  await page.evaluate(() => document.documentElement.setAttribute('data-mantine-color-scheme', 'dark'))
}

let world: World
// Next to the stack (test-results is emptied at the start of every run).
const STATE = join(process.env.SM_DIR ?? 'test-results', 'overflow-state.json')

test.describe.configure({ mode: 'default' })

// Seeded once per stack: Playwright restarts the worker after a failed test, and the next test reuses the world and the
// session.
test.beforeAll(async ({ browser }) => {
  test.setTimeout(240_000)
  // A context without this file's storage state (written here); the tests reuse the session through it.
  const file = join(env('SM_DIR'), 'overflow-world.json')
  if (existsSync(file) && existsSync(STATE)) {
    world = JSON.parse(readFileSync(file, 'utf8')) as World
    return
  }
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } })
  world = await seedWorld(await ctx.newPage(), Date.now().toString(36))
  await ctx.storageState({ path: STATE })
  writeFileSync(file, JSON.stringify(world))
  await ctx.close()
})

test.afterAll(async ({ browser }) => {
  if (!world) return
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } })
  await stopWorld(await ctx.newPage(), world)
  await ctx.close()
})

test.use({ storageState: STATE })

/**
 * The three widths are independent (own browser context, same session and world, nothing is written), so they run at
 * the same time: a test takes as long as its slowest width instead of the sum. Every page and every dialog is still
 * checked at every width in both themes; only the waiting overlaps.
 */
async function atEachWidth(browser: Browser, run: (page: Page, width: number) => Promise<string[]>) {
  const failures = await Promise.all(
    WIDTHS.map(async (width) => {
      const ctx = await browser.newContext({ storageState: STATE, viewport: { width, height: height(width) }, colorScheme: 'dark' })
      try {
        return await run(await ctx.newPage(), width)
      } finally {
        await ctx.close()
      }
    }),
  )
  expect(failures.flat()).toEqual([])
}

test('pages have no unwanted scrollbars at phone, desktop and wide widths', async ({ browser }) => {
  test.setTimeout(300_000)
  await atEachWidth(browser, async (page, width) => {
    const failures: string[] = []
    for (const path of PAGES(world)) {
      await page.goto(path)
      await page.locator('main').waitFor()
      await settle(page, 600)
      await check(page, `${path} at ${width} px`, failures)
    }
    return failures
  })
})

test('every modal and drawer has no unwanted scrollbars at phone, desktop and wide widths', async ({ browser }) => {
  test.setTimeout(600_000)
  await atEachWidth(browser, async (page, width) => {
    const failures: string[] = []
    const dialogs = page.locator('.mantine-Modal-content, .mantine-Drawer-content')
    let at = ''
    for (const d of DIALOGS) {
      if (d.widths && !d.widths.includes(width)) continue
      // Dialogs on the same page reuse it (closed with Escape); a fresh load otherwise.
      const path = d.path(world)
      if (path !== at || !page.url().endsWith(path) || (await dialogs.count()) > 0) {
        await page.goto(path)
        await page.locator('main').waitFor()
        await settle(page, 400)
        at = path
      }
      try {
        for (const step of d.open) {
          await step(page)
          await settle(page, 300)
        }
        await expect(dialogs.last()).toBeVisible(T)
      } catch (e) {
        failures.push(`${d.name} at ${width} px: could not open (${String(e).split('\n')[0]})`)
        at = ''
        continue
      }
      await settle(page, 450) // Mantine transitions run in JavaScript
      await check(page, `${d.name} at ${width} px`, failures)
      for (let i = 0; i < 3 && (await dialogs.count()) > 0; i++) {
        await page.keyboard.press('Escape')
        await settle(page, 350)
      }
    }
    return failures
  })
})

/**
 * Operator report after #29 (Chrome on macOS, Retina): the "New inbox item" dialog showed a vertical scrollbar although
 * its content fit, about 100 px of nothing below the footer. Cause: the Grammarly extension, which inserts absolutely
 * positioned overlay nodes (`grammarly-extension`) next to a contenteditable. A Mantine modal's content keeps an identity
 * transform after its transition, so it is their containing block, fixed ones included, and it scrolled by however far
 * they reached. The editor now contains them (`contain: layout` on .sm-composer). Checked at the operator's size and at
 * short windows, at DPR 2, with a node shaped like Grammarly's and with other overlays placed by page or viewport
 * coordinates.
 */
test('the inbox item dialog does not scroll when its content fits, even with nodes injected next to the editor', async ({ browser }) => {
  test.setTimeout(120_000)
  for (const viewport of [{ width: 1600, height: 840 }, { width: 1440, height: 700 }, { width: 1280, height: 600 }, { width: 900, height: 500 }]) {
    const ctx = await browser.newContext({ storageState: STATE, viewport, deviceScaleFactor: 2, colorScheme: 'dark' })
    const page = await ctx.newPage()
    const at = `${viewport.width}x${viewport.height}@2x`
    await page.goto(`${space(world)}/inbox`)
    await button('New item')(page)
    const dialog = page.locator('.mantine-Modal-content').last()
    await expect(dialog).toBeVisible()
    await settle(page, 450)
    expect(await overflowProblems(page), at).toEqual([])
    expect(await dialog.evaluate((e) => e.scrollHeight - e.clientHeight), at).toBe(0)
    const extra = await dialog.evaluate((c) => {
      const editor = c.querySelector('.ProseMirror') as HTMLElement
      const { top, bottom } = c.getBoundingClientRect()
      const nodes: [string, string][] = [
        // Grammarly's shape: a zero-size absolute host at its containing block's corner, holding an overlay placed in
        // that box's coordinates (the modal content's, without containment), here about 100 px below the editor.
        ['grammarly-extension', `position:absolute;top:0;left:0;pointer-events:none`],
        // Overlays placed by page and by viewport coordinates, just below the dialog.
        ['div', `position:absolute;left:300px;top:${bottom + 16}px;width:20px;height:40px`],
        ['div', `position:fixed;left:300px;top:${bottom + 16}px;width:20px;height:40px`],
      ]
      return nodes.map(([tag, css]) => {
        const node = document.createElement(tag)
        node.style.cssText = css
        if (tag === 'grammarly-extension') {
          const overlay = document.createElement('div')
          overlay.style.cssText = `position:absolute;left:0;top:${editor.getBoundingClientRect().bottom - top + 100}px;width:200px;height:24px`
          node.appendChild(overlay)
        }
        editor.parentElement!.insertBefore(node, editor)
        const over = c.scrollHeight - c.clientHeight
        node.remove()
        return over
      })
    })
    expect(extra, `${at}: injected nodes extend the dialog's scroll area`).toEqual([0, 0, 0])
    await ctx.close()
  }
})

test('a dialog taller than the screen still scrolls', async ({ page }) => {
  test.setTimeout(120_000)
  // A long form on a phone.
  await page.setViewportSize({ width: 390, height: 640 })
  await page.goto(`${space(world)}/queue`)
  await button('New task')(page)
  const content = page.locator('.mantine-Modal-content').last()
  await expect(content).toBeVisible()
  await settle(page, 450)
  const before = await content.evaluate((e) => ({ sh: e.scrollHeight, ch: e.clientHeight, sw: e.scrollWidth, cw: e.clientWidth }))
  expect(before.sh, 'the form is taller than the dialog').toBeGreaterThan(before.ch)
  expect(before.sw, 'and no wider').toBeLessThanOrEqual(before.cw)
  await content.evaluate((e) => e.scrollTo(0, e.scrollHeight))
  await expect.poll(() => content.evaluate((e) => e.scrollTop)).toBeGreaterThan(0)
  await expect(page.getByRole('dialog').getByRole('button', { name: 'Cancel' })).toBeInViewport()

  // The inbox item dialog fits on a desktop, then grows with a long body until it scrolls.
  await page.setViewportSize({ width: 1280, height: 700 })
  await page.goto(`${space(world)}/inbox`)
  await button('New item')(page)
  const dialog = page.locator('.mantine-Modal-content').last()
  await expect(dialog).toBeVisible()
  await settle(page, 450)
  expect(await overflowProblems(page)).toEqual([])
  expect(await dialog.evaluate((e) => e.scrollHeight - e.clientHeight)).toBe(0)
  await page.getByRole('textbox', { name: 'Item text' }).click()
  for (let i = 0; i < 30; i++) await page.keyboard.press('Enter')
  await page.keyboard.type('the last line')
  await expect.poll(() => dialog.evaluate((e) => e.scrollHeight - e.clientHeight)).toBeGreaterThan(0)
  expect(await overflowProblems(page)).toEqual([])
  await dialog.evaluate((e) => e.scrollTo(0, e.scrollHeight))
  await expect(page.getByRole('dialog').getByRole('button', { name: 'Add to inbox' })).toBeInViewport()
})

/**
 * Operator report (iPhone, v0.1.6): the account dialog ran off the right edge, with no way to scroll sideways or to pinch
 * out. The desktop-width runs above never saw it: a narrow desktop window is not a phone. A phone (mobile emulation, touch,
 * device pixel ratio 3) grows its layout viewport to fit wide content, so a dialog wider than the screen cut itself off
 * and zoomed the page out. Every modal and drawer is opened on a phone and must lie inside the screen with nothing cut
 * off, and every text control in it is at least 16 px (iOS zooms in on a smaller one); pinch-zoom stays allowed while one is
 * open and once it is closed. SM_PHONE_SHOTS=<dir> saves screenshots.
 */
test('every modal and drawer fits a phone and leaves pinch-zoom alone', async ({ browser }) => {
  test.setTimeout(600_000)
  const { defaultBrowserType: _ignored, ...phone } = devices['iPhone 13']
  void _ignored
  const ctx = await browser.newContext({ ...phone, viewport: { width: 390, height: 844 }, storageState: STATE, colorScheme: 'dark' })
  const page = await ctx.newPage()
  const failures: string[] = []
  const dialogs = page.locator('.mantine-Modal-content, .mantine-Drawer-content')
  const shots = process.env.SM_PHONE_SHOTS
  let at = ''
  for (const d of DIALOGS) {
    if (d.widths && !d.widths.includes(390)) continue
    const path = d.path(world)
    if (path !== at || !page.url().endsWith(path) || (await dialogs.count()) > 0) {
      await page.goto(path)
      await page.locator('main').waitFor()
      await settle(page, 400)
      at = path
    }
    try {
      for (const step of d.open) {
        await step(page)
        await settle(page, 300)
      }
      await expect(dialogs.last()).toBeVisible(T)
    } catch (e) {
      failures.push(`${d.name}: could not open (${String(e).split('\n')[0]})`)
      at = ''
      continue
    }
    await settle(page, 450)
    for (const p of await phoneProblems(page)) failures.push(`${d.name}: ${p}`)
    for (const p of await smallTouchText(page)) failures.push(`${d.name}: text control under 16 px on a touch screen: ${p}`)
    for (const p of await zoomBlockers(page)) failures.push(`${d.name} (open): ${p}`)
    if (await pinchCancelled(page)) failures.push(`${d.name}: a pinch on the open dialog is cancelled (the scroll lock blocks zoom)`)
    if (shots && /^(Edit account|Add account|New task)$/.test(d.name)) await page.screenshot({ path: join(shots, `${d.name.replace(/\W+/g, '-').toLowerCase()}.png`) })
    for (let i = 0; i < 3 && (await dialogs.count()) > 0; i++) {
      await page.keyboard.press('Escape')
      await settle(page, 350)
    }
  }
  // Closed again: the scroll lock is released and zoom is not blocked.
  expect(await dialogs.count()).toBe(0)
  expect(await zoomBlockers(page), 'after the last dialog closed').toEqual([])
  expect(await page.evaluate(() => [document.body.style.overflow, document.body.style.position, document.documentElement.style.overflow])).toEqual(['', '', ''])
  await ctx.close()
  expect(failures).toEqual([])
})
