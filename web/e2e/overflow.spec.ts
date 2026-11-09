import { expect, test, type Page } from '@playwright/test'

import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { overflowProblems, realScrollbars, seedWorld, stopWorld, type World } from './overflow'
import { env, settle } from './stack'

/**
 * No unwanted scrollbars (operator report: the "New inbox item" dialog showed both scrollbars although its content fit,
 * and the space sections row showed a vertical one). Every page, modal and drawer is opened at phone (390 px), desktop
 * (1280 px) and wide (1920 px) widths, in both themes, with real scrollbars (Playwright's Chromium hides them by default,
 * which hides this class of bug), and checked by `overflowProblems`: no page or dialog that scrolls sideways, no modal that
 * scrolls although its content fits, no scroll container that overflows by a sliver. A long form still scrolls.
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
  '/', '/attention', '/human', '/terminals', '/spaces', '/nodes', '/prompts', `/prompts/${w.prompt}`, '/roles', '/skills', '/approval-policies', '/human-types',
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

for (const width of WIDTHS) {
  test(`pages at ${width} px have no unwanted scrollbars`, async ({ page }) => {
    test.setTimeout(300_000)
    await page.setViewportSize({ width, height: height(width) })
    const failures: string[] = []
    for (const path of PAGES(world)) {
      await page.goto(path)
      await page.locator('main').waitFor()
      await settle(page, 600)
      await check(page, `${path} at ${width} px`, failures)
    }
    expect(failures).toEqual([])
  })

  test(`every modal and drawer at ${width} px has no unwanted scrollbars`, async ({ page }) => {
    test.setTimeout(600_000)
    await page.setViewportSize({ width, height: height(width) })
    const failures: string[] = []
    const dialogs = page.locator('.mantine-Modal-content, .mantine-Drawer-content')
    let at = ''
    for (const d of DIALOGS) {
      if (d.widths && !d.widths.includes(width)) continue
      await test.step(d.name, async () => {
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
          return
        }
        await settle(page, 450) // Mantine transitions run in JavaScript
        await check(page, `${d.name} at ${width} px`, failures)
        for (let i = 0; i < 3 && (await dialogs.count()) > 0; i++) {
          await page.keyboard.press('Escape')
          await settle(page, 350)
        }
      })
    }
    expect(failures).toEqual([])
  })
}

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
