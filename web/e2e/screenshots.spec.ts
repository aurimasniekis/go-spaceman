import { randomBytes } from 'node:crypto'
import { mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

import { expect, test, type Browser, type Page } from '@playwright/test'

import { Api, env, settle, startAgent } from './stack'

/**
 * Regenerates the documentation screenshots (site/public/screenshots, inventory in docs/SCREENSHOTS.md) from the real UI
 * on a fresh `scripts/dev-stack.sh` stack seeded with fictional data: example.com people, invented spaces and a stub
 * harness, so no real model, account or credential appears. Every shot is captured in Night Owl (dark) and Light Owl
 * (light) at 1440×900. Run it through scripts/screenshots.sh.
 */

const out = process.env.SM_SCREENSHOTS_OUT
test.skip(!out, 'SM_SCREENSHOTS_OUT is not set (use scripts/screenshots.sh)')

const THEMES = ['dark', 'light'] as const
const password = () => `demo-${randomBytes(18).toString('base64url')}`

async function setTheme(page: Page, theme: (typeof THEMES)[number]) {
  await page.evaluate((t) => localStorage.setItem('mantine-color-scheme-value', t), theme)
}

/** Captures `name` in both themes: `prepare` opens the page (and any dialog) after the theme is set. */
async function capture(page: Page, name: string, prepare: () => Promise<void>) {
  for (const theme of THEMES) {
    await setTheme(page, theme)
    await prepare()
    await settle(page, 500)
    await page.screenshot({ path: join(out!, `${name}-${theme}.png`), animations: 'disabled' })
    await page.keyboard.press('Escape')
  }
}

async function enrollmentShot(browser: Browser, email: string, pw: string) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } })
  const page = await ctx.newPage()
  await page.goto('/login')
  await capture(page, 'setup-mfa', async () => {
    await page.goto('/login')
    await page.getByLabel('Email').fill(email)
    await page.getByLabel('Password').fill(pw)
    await page.getByRole('button', { name: 'Sign in' }).click()
    await expect(page.getByText('Set up two-factor authentication')).toBeVisible()
    await expect(page.getByRole('img', { name: 'Authenticator QR code' })).toBeVisible()
    // Each capture starts a fresh sign-in.
    await ctx.clearCookies()
  })
  await ctx.close()
}

test('documentation screenshots', async ({ page, browser }) => {
  test.setTimeout(300_000)
  mkdirSync(out!, { recursive: true })
  const base = env('SM_URL')
  const root = readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim()

  // First run: the setup page, before any administrator exists.
  await page.goto('/login')
  await capture(page, 'setup-bootstrap', async () => {
    await page.goto('/setup')
    await page.getByLabel('Root token').fill(root)
    await page.getByLabel('Name').fill('Alex Morgan')
    await page.getByLabel('Email').fill('alex@example.com')
  })

  // Seed through the API, as the root token and then as the administrator (the browser shares the session).
  const asRoot = new Api(page.request, base, root)
  const adminPw = password()
  await asRoot.call('POST', '/users', { display_name: 'Alex Morgan', email: 'alex@example.com', password: adminPw, mfa_required: true, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('alex@example.com', adminPw)

  const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'build-01', description: 'Workstation in the office rack' })
  await api.call('POST', '/nodes', { name: 'gpu-02', description: 'Spare machine, not enrolled yet' })
  startAgent(node.token)
  await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${node.node.id}`)).connected, { timeout: 30_000 }).toBe(true)

  type Space = { id: string; version: number }
  const mk = (slug: string, name: string, description: string, goals: string) =>
    api.call<Space>('POST', '/spaces', { slug, name, description, goals, node_id: node.node.id, standing_instructions: 'Open a pull request for every change. Never push to main.' })
  const payments = await mk('payments', 'Payments platform', 'Card processing services and their release train', 'Keep the settlement pipeline green; ship the retry-budget change this week.')
  const docs = await mk('docs-site', 'Docs site', 'Public documentation and examples', 'Every guide has current screenshots and tested examples.')
  const infra = await mk('infra', 'Infrastructure', 'Terraform for the staging environment', 'Move staging to the new VPC without downtime.')
  await api.call('POST', `/spaces/${infra.id}/state`, { state: 'paused', mode: 'hold' }, { 'If-Match': `"${infra.version}"` })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${payments.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${docs.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  type Run = { id: string }
  const launch = (space: string, kind: string, role: string, harness: string, prompt: string) =>
    api.call<Run>('POST', `/spaces/${space}/runs`, { kind, role, harness, prompt })
  const control = await launch(payments.id, 'control', 'control', 'claude-code', 'Coordinate the retry-budget change across the settlement services.')
  const reviewer = await launch(payments.id, 'worker', 'reviewer', 'codex', 'Review the open pull requests and summarise the risky ones.')
  const finished = await launch(payments.id, 'worker', 'implementor-backend', 'claude-code', 'Add the retry budget to the settlement client.')
  await launch(docs.id, 'control', 'control', 'claude-code', 'Keep the guides in step with the UI.')
  const running = async (space: string, id: string) => (await api.call<{ observed: string }>('GET', `/spaces/${space}/runs/${id}`)).observed
  for (const r of [control, reviewer, finished]) await expect.poll(() => running(payments.id, r.id), { timeout: 30_000 }).toBe('running')
  await page.waitForTimeout(6_000) // a few seconds of output to show
  await api.call('POST', `/spaces/${payments.id}/runs/${finished.id}/stop`, { mode: 'kill', reason: 'finished the change' })
  await expect.poll(() => running(payments.id, finished.id), { timeout: 30_000 }).toMatch(/exited|failed/)

  // Inbox: operator requests (one with a real screenshot of the stopped run and its log), an alert as JSON, and items in
  // every state.
  await page.goto(`/spaces/${payments.id}/runs/${finished.id}?view=replay`)
  await expect(page.getByRole('button', { name: 'Play replay' })).toBeEnabled({ timeout: 20_000 })
  await setTheme(page, 'dark')
  await page.reload()
  await expect(page.getByRole('button', { name: 'Play replay' })).toBeEnabled({ timeout: 20_000 })
  await settle(page, 800)
  const terminalShot = await page.locator('.sm-terminal').screenshot()
  const img = await api.upload(payments.id, 'settlement-run.png', 'image/png', terminalShot)
  const log = await api.upload(payments.id, 'settlement-job.log', 'text/plain',
    Buffer.from('2026-11-01T02:14:07Z attempt 1 failed: upstream timeout\n2026-11-01T02:19:07Z attempt 2 failed: upstream timeout\n2026-11-01T02:24:07Z attempt 3 failed: giving up\n'))
  type Item = { id: string; version: number }
  const inbox = (body: Record<string, unknown>) => api.call<Item>('POST', `/spaces/${payments.id}/inbox`, body)
  const setStatus = (it: Item, body: Record<string, unknown>) => api.call<Item>('POST', `/spaces/${payments.id}/inbox/${it.id}/status`, body, { 'If-Match': `"${it.version}"` })
  const done = await inbox({ body: 'Add the retry budget to the settlement client\n\nUse the budget from the payments RFC: at most **3** attempts per batch.' })
  await setStatus(done, { status: 'done', linked_kind: 'run', linked_id: finished.id })
  const deferred = await inbox({ body: 'Rotate the staging webhook secret', urgency: 'low' })
  await setStatus(deferred, { status: 'deferred', reason: 'after the release freeze', defer_until: new Date(Date.now() + 5 * 86_400_000).toISOString() })
  const sdk = await inbox({ body: 'Bump the card SDK to 7.2\n\nThe changelog mentions a fix for duplicate refunds.' })
  await setStatus(sdk, { status: 'in_progress' })
  await inbox({ title: 'Settlement lag alert', content_type: 'application/json', urgency: 'urgent',
    body: JSON.stringify({ alert: 'settlement_lag', environment: 'production', lag_seconds: 840, threshold_seconds: 300 }) })
  const weekend = await inbox({ urgency: 'high', attachment_ids: [img.id, log.id],
    body: `Settlement retries fail on weekends\n\nThe batch job gives up after **three** attempts while the bank's API is in maintenance.\n\n- [ ] Confirm the maintenance window\n- [ ] Make the retry budget time-based\n\n![The stopped run](${img.uri})\n\n[settlement-job.log](${log.uri})` })

  // People and tokens.
  const samPw = password()
  await api.call('POST', '/users', { display_name: 'Sam Rivera', email: 'sam@example.com', password: samPw, mfa_required: true, grants: [{ role: 'operator', domain: payments.id }] })
  await api.call('POST', '/users', { display_name: 'Jordan Lee', email: 'jordan@example.com', password: password(), mfa_required: true, grants: [{ role: 'viewer', domain: '*' }] })
  await api.call('POST', '/tokens', { name: 'terraform-ci', grants: [{ role: 'admin', domain: '*' }], expires_in_days: 90 })
  await api.call('POST', '/tokens', { name: 'grafana-readonly', grants: [{ role: 'viewer', domain: '*' }], never_expires: true })
  await enrollmentShot(browser, 'sam@example.com', samPw)

  await page.goto('/')
  await capture(page, 'landing-overview', async () => {
    await page.goto('/')
    await expect(page.getByRole('heading', { name: 'Overview' })).toBeVisible()
    await expect(page.getByText('build-01').first()).toBeVisible()
  })
  await capture(page, 'space-overview', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await expect(page.getByText('reviewer')).toBeVisible()
  })
  await capture(page, 'space-create', async () => {
    await page.goto('/spaces')
    await page.getByRole('button', { name: 'New space' }).click()
    await page.getByLabel('Name').fill('Mobile app')
    await page.getByLabel('Slug').fill('mobile')
    await page.getByRole('textbox', { name: 'Pinned node' }).click()
    await page.getByRole('option', { name: 'build-01' }).click()
    await page.getByLabel('Description', { exact: true }).fill('iOS and Android clients')
    await page.getByLabel('Goals').fill('Ship offline mode in the next release.')
  })
  await capture(page, 'run-launch', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await page.getByRole('button', { name: 'Launch run' }).click()
    await page.getByLabel('Role').fill('implementor-frontend')
    await page.getByLabel('Initial prompt').fill('Show the retry budget on the merchant dashboard.')
  })
  await capture(page, 'terminal-live', async () => {
    await page.goto(`/spaces/${payments.id}/runs/${control.id}`)
    await expect(page.getByText('Live · watch-only')).toBeVisible()
    await page.waitForTimeout(1_500)
  })
  await capture(page, 'terminal-replay', async () => {
    await page.goto(`/spaces/${payments.id}/runs/${finished.id}?view=replay`)
    await expect(page.getByRole('button', { name: 'Play replay' })).toBeEnabled({ timeout: 20_000 })
  })
  await capture(page, 'inbox-list', async () => {
    await page.goto(`/spaces/${payments.id}/inbox`)
    await page.getByRole('radiogroup', { name: 'Filter by status' }).getByText('All', { exact: true }).click()
    await expect(page.getByText('Settlement lag alert')).toBeVisible()
  })
  await capture(page, 'inbox-item', async () => {
    await page.goto(`/spaces/${payments.id}/inbox/${weekend.id}`)
    await expect(page.locator('.sm-markdown img')).toBeVisible()
    await page.waitForTimeout(500)
  })
  await capture(page, 'inbox-composer', async () => {
    await page.goto(`/spaces/${payments.id}/inbox`)
    await page.getByRole('button', { name: 'New item' }).click()
    await page.getByRole('textbox', { name: 'Urgency' }).click()
    await page.getByRole('option', { name: 'High' }).click()
    const editor = page.getByRole('textbox', { name: 'Item text' })
    await editor.click()
    await page.keyboard.type('Refunds are posted twice for split payments')
    await page.keyboard.press('Enter')
    await page.keyboard.type('Seen by two merchants since Tuesday. The ')
    await page.keyboard.press('ControlOrMeta+e')
    await page.keyboard.type('refund_id')
    await page.keyboard.press('ControlOrMeta+e')
    await page.keyboard.type(' differs, the amount does not.')
    await page.locator('input[type=file]').setInputFiles([{ name: 'refunds-export.csv', mimeType: 'text/csv', buffer: Buffer.from('refund_id,amount\nrf_1,12.00\nrf_2,12.00\n') }])
    await expect(page.getByText('refunds-export.csv ·')).toBeVisible()
  })
  await capture(page, 'setup-node-enroll', async () => {
    await page.goto('/nodes')
    await expect(page.getByText('connected', { exact: true })).toBeVisible()
  })
  await capture(page, 'auth-users', async () => {
    await page.goto('/users')
    await expect(page.getByText('Sam Rivera')).toBeVisible()
  })
  await capture(page, 'auth-tokens', async () => {
    await page.goto('/tokens')
    await expect(page.getByText('terraform-ci').first()).toBeVisible()
  })
  await capture(page, 'audit-log', async () => {
    await page.goto('/audit')
    await expect(page.getByText('run.launched').first()).toBeVisible()
  })
})
