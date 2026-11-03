import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, test, type Page } from '@playwright/test'

import { Api, settle } from './stack'

/**
 * The browser steps of the scripted demonstration (test/demo, `make demo`, docs/DEMO.md). The Go driver starts the stack
 * (a real central with the embedded UI, real node agents with their own herdr-core cores) and runs this spec once per
 * phase, with SM_DEMO_DIR pointing at its run directory:
 *
 *   SM_DEMO_PHASE=space    sign in as the demo administrator (TOTP enrollment) and create a space pinned to a node
 *   SM_DEMO_PHASE=respond  answer a worker's question from the global "Needs you" page
 *
 * Inputs and outputs are small JSON files in SM_DEMO_DIR (0600). Screenshots of what the browser saw go to
 * SM_DEMO_DIR/shots for the run's evidence; they are not committed.
 */

const phase = process.env.SM_DEMO_PHASE ?? ''
const dir = process.env.SM_DEMO_DIR ?? ''
test.skip(!phase || !dir, 'SM_DEMO_PHASE and SM_DEMO_DIR are set by the demo driver (make demo)')

const read = <T>(name: string): T => JSON.parse(readFileSync(join(dir, name), 'utf8')) as T
const write = (name: string, v: unknown) => writeFileSync(join(dir, name), JSON.stringify(v), { mode: 0o600 })
const statePath = () => join(dir, 'ui-state.json')

async function shot(page: Page, name: string) {
  mkdirSync(join(dir, 'shots'), { recursive: true })
  await settle(page, 500)
  await page.screenshot({ path: join(dir, 'shots', `${name}.png`), animations: 'disabled' })
}

test('space: UI creation of a pinned space', async ({ page }) => {
  test.skip(phase !== 'space')
  test.setTimeout(180_000)
  const input = read<{ email: string; password: string; node: string; name: string; slug: string; goals: string }>('ui-space-in.json')
  const api = new Api(page.request, process.env.SM_URL ?? '')
  await api.signIn(input.email, input.password)
  await page.context().storageState({ path: statePath() })

  await page.goto('/spaces')
  await page.getByRole('button', { name: 'New space' }).click()
  await page.getByLabel('Name').fill(input.name)
  await page.getByLabel('Slug').fill(input.slug)
  await page.getByRole('textbox', { name: 'Pinned node' }).click()
  await page.getByRole('option', { name: input.node }).click()
  await page.getByLabel('Goals').fill(input.goals)
  await shot(page, '01-space-form')
  await page.getByRole('button', { name: 'Create space' }).click()
  await expect(page.getByRole('heading', { name: new RegExp(input.name) })).toBeVisible()
  await expect(page).toHaveURL(/\/spaces\/spc_/)
  const id = /\/spaces\/(spc_[A-Za-z0-9]+)/.exec(page.url())![1]
  // The lease is granted by central on the node's next heartbeat.
  await expect(page.getByText('valid')).toBeVisible({ timeout: 30_000 })
  await shot(page, '01-space-created')
  await page.context().storageState({ path: statePath() })
  write('ui-space-out.json', { id, url: page.url() })
})

test('respond: a global operator response', async ({ browser }) => {
  test.skip(phase !== 'respond')
  test.setTimeout(120_000)
  const input = read<{ title: string; answer: string; space: string }>('ui-respond-in.json')
  const ctx = await browser.newContext({ storageState: statePath(), viewport: { width: 1440, height: 900 } })
  const page = await ctx.newPage()
  await page.goto('/')
  await page.getByRole('link', { name: /^Needs you/ }).click()
  await expect(page.getByRole('heading', { name: 'Needs you' })).toBeVisible()
  const row = page.getByRole('listitem').filter({ hasText: input.title })
  await expect(row).toBeVisible({ timeout: 30_000 })
  await expect(row).toContainText(input.space)
  await shot(page, '16-needs-you')
  await row.click()
  await page.getByRole('radio', { name: input.answer }).check()
  await shot(page, '16-answer-form')
  await page.getByRole('button', { name: 'Send answer' }).click()
  await expect(page.getByText('Response: answered')).toBeVisible()
  await shot(page, '16-answered')
  write('ui-respond-out.json', { answered: true })
  await ctx.close()
})
