import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

import AxeBuilder from '@axe-core/playwright'
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
// Accessibility (SM-UI-006, SM-DOCS-005): every captured page is audited with axe in both themes against WCAG 2.1 A/AA.
// Violations are collected and reported together at the end, so one run lists everything to fix.
const a11y: string[] = []

async function audit(page: Page, name: string, theme: string) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  for (const v of res.violations) {
    const nodes = v.nodes.slice(0, 3).map((n) => `${n.target.join(' ')}${v.id === 'color-contrast' ? ` {${n.any[0]?.message ?? ''}}` : ''}`)
    a11y.push(`${name} (${theme}): ${v.id} [${v.impact}] ${v.help}: ${nodes.join(' | ')}`)
  }
}

async function capture(page: Page, name: string, prepare: () => Promise<void>) {
  for (const theme of THEMES) {
    await setTheme(page, theme)
    await prepare()
    await settle(page, 500)
    await page.screenshot({ path: join(out!, `${name}-${theme}.png`), animations: 'disabled' })
    await audit(page, name, theme)
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

  // Accounts: a subscription token and an API key for Claude Code, an OpenAI key for Codex (stub values; the stub harness
  // ignores them), bound to the active spaces with defaults.
  type Acc = { id: string }
  const account = (name: string, kind: string, description: string, enabled = true) =>
    api.call<Acc>('POST', '/accounts', { name, kind, description, enabled, secret: `stub-${name}` })
  const teamClaude = await account('team-claude', 'claude_oauth_token', 'Team subscription (claude setup-token)')
  const ciAnthropic = await account('ci-anthropic', 'anthropic_api_key', 'Metered API key for CI spaces')
  const teamCodex = await account('team-codex', 'openai_api_key', 'Team OpenAI project key')
  await account('old-claude', 'claude_oauth_token', 'Replaced in October', false)
  for (const sp of [payments, docs]) {
    await api.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: teamClaude.id, default: true }, { account_id: ciAnthropic.id }, { account_id: teamCodex.id, default: true }] })
  }

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

  // Queue: work in every state, an incident an operator moved down, a dependency and an explicit blocker.
  type T = { id: string; version: number }
  const task = (body: Record<string, unknown>) => api.call<T>('POST', `/spaces/${payments.id}/tasks`, { status: 'ready', ...body })
  const sdk2 = await task({ title: 'Bump the card SDK to 7.2', objective: 'Upgrade and run the refund regression suite.', exclusions: ['repo:payments-api'] })
  await api.call('POST', `/spaces/${payments.id}/tasks/${sdk2.id}/status`, { status: 'running' }, { 'If-Match': `"${sdk2.version}"` })
  const budget = await task({ title: 'Make the settlement retry budget time-based', priority: 'high', from_inbox_item: weekend.id,
    acceptance: '- [ ] Retries span the bank maintenance window\n- [ ] Alert when the budget is exhausted' })
  const lag = await task({ title: 'Settlement lag above 10 minutes', kind: 'incident', priority: 'urgent', objective: 'Find why settlement batches queue up.' })
  await api.call('POST', `/spaces/${payments.id}/tasks/${lag.id}/move`, { after_id: budget.id })
  const exportTask = await task({ title: 'Refund export for finance', objective: 'Monthly CSV export of refunds.' })
  await api.call('POST', `/spaces/${payments.id}/tasks/${exportTask.id}/blockers`, { kind: 'external', note: 'waiting for the finance team to confirm the columns' })
  await task({ title: 'Load-test the new retry budget', depends_on: [budget.id], exclusions: ['env:staging'] })
  await task({ title: 'Remove stale feature flags', priority: 'low', status: 'backlog' })

  // Human actions: one of each type (a blocking question on a task), one in another space, and an answered one.
  const human = (space: string, body: Record<string, unknown>) => api.call<T>('POST', `/spaces/${space}/human-actions`, body)
  await human(payments.id, { type: 'question', title: 'Should retries span the whole bank maintenance window?', task_id: budget.id, blocks_task: true, priority: 'high',
    body: 'The window is up to **6 hours** on Sundays. Spanning it means a batch can settle late instead of failing.',
    request: { options: ['Span the whole window', 'Cap at 2 hours, then alert'], allow_text: true } })
  await human(payments.id, { type: 'approval', title: 'Deploy the settlement retry change to production', request: { action: 'Deploy payments-api v2.14.0 to production',
    details: 'Changes the retry budget from three attempts to a time-based budget. Rollback: redeploy v2.13.4.' } })
  await human(payments.id, { type: 'todo', title: "Confirm the bank's maintenance window", request: { steps: ['Email the bank integration contact', 'Record the window in the runbook'] } })
  await human(payments.id, { type: 'message', title: 'Weekend settlement runs are paused until Monday', request: { requires_ack: false }, priority: 'low' })
  await human(docs.id, { type: 'todo', title: 'Review the wording of the refunds API reference' })
  // Messages from operators: a request to control, a note to every reviewer, one resolved.
  const msg = (space: string, body: Record<string, unknown>) => api.call<{ id: string }>('POST', `/spaces/${space}/messages`, body)
  await msg(payments.id, { to: 'control', subject: 'Hold deploys until the bank window closes', request: true, body: 'The window ends at **06:00 UTC** on Monday.' })
  await msg(payments.id, { to: 'role:reviewer', subject: 'Check refund idempotency in every review', body: 'Duplicate refunds were reported twice this week.' })
  const old = await msg(payments.id, { to: 'control', subject: 'Rotate the staging webhook secret after the freeze' })
  await api.call('POST', `/spaces/${payments.id}/messages/${old.id}/resolve`, { resolution: 'done on Friday' })
  const answered = await human(payments.id, { type: 'question', title: 'Which currency should the refund export use?', request: { options: ['EUR', 'Original currency'] } })
  await api.call('POST', `/spaces/${payments.id}/human-actions/${answered.id}/respond`, { outcome: 'answered', answer: 'Original currency' }, { 'If-Match': `"${answered.version}"` })

  // Events: a Pulse connection (devpulse), GitHub issue subscriptions and published go-pulse-github events (one for another
  // repository, filtered), and an alerts subscription with a schema that quarantines a malformed alert.
  const pulseDir = env('SM_PULSE_DIR')
  const conn = await api.call<{ id: string }>('POST', '/event-connections', { name: 'pulse-prod', url: readFileSync(join(pulseDir, 'url'), 'utf8').trim(),
    creds: readFileSync(join(pulseDir, 'spaceman.creds'), 'utf8'), enabled: true })
  const catalog = await api.call<{ items: { slug: string; versions: { id: string }[] }[] }>('GET', '/event-templates')
  const issueTpl = catalog.items.find((t) => t.slug === 'github-issue')!
  const issues = await api.call<{ id: string }>('POST', `/spaces/${payments.id}/subscriptions`, { name: 'settlement-service issues', connection_id: conn.id,
    template_version_id: issueTpl.versions[0].id, follow: 'latest', vars: { account_key: 'example-org', repository: 'example-org/settlement-service' } })
  await api.call('POST', `/spaces/${docs.id}/subscriptions`, { name: 'docs issues', connection_id: conn.id, template_version_id: issueTpl.versions[0].id,
    vars: { account_key: 'example-org', repository: 'example-org/docs' } })
  const alerts = await api.call<{ id: string }>('POST', `/spaces/${payments.id}/subscriptions`, { name: 'production alerts', connection_id: conn.id,
    custom: { subject: 'evt.ops.alert.production', variables: [], filters: [], title: '{{data.name}} ({{data.severity}})', urgency: 'urgent',
      body: '**{{data.name}}** fired: {{data.summary | default "(no summary)"}}', instructions: 'A production alert fired for the payments services.',
      input_schema: { type: 'object', required: ['name', 'severity'] } } })
  await expect.poll(async () => (await api.call<{ state: string }>('GET', `/spaces/${payments.id}/subscriptions/${issues.id}`)).state, { timeout: 30_000 }).toBe('running')
  await expect.poll(async () => (await api.call<{ state: string }>('GET', `/spaces/${payments.id}/subscriptions/${alerts.id}`)).state, { timeout: 30_000 }).toBe('running')
  const sampleFile = join(process.cwd(), '..', 'internal/events/template/testdata/pulse-github-issues-opened.json')
  const sampleBase = JSON.parse(readFileSync(sampleFile, 'utf8')) as Record<string, any>
  const tmp = join(env('SM_DIR'), 'events')
  mkdirSync(tmp, { recursive: true })
  const publish = (subject: string, type: string, data: unknown) => {
    const file = join(tmp, `${Math.random().toString(36).slice(2)}.json`)
    writeFileSync(file, JSON.stringify(data))
    execFileSync(join(env('SM_BIN'), 'devpulse'), ['publish', '-dir', pulseDir, '-subject', subject, '-type', type, file])
  }
  const issueEvent = (repo: string, number: number, action: string, title: string, body: string) => {
    const e = structuredClone(sampleBase)
    e.repository.full_name = repo
    e.resource.number = number
    e.resource.html_url = `https://github.com/${repo}/issues/${number}`
    e.github.action = action
    e.payload.action = action
    e.payload.issue.number = number
    e.payload.issue.title = title
    e.payload.issue.body = body
    e.actor.login = 'sam-rivera'
    return e
  }
  publish('evt.github.example-org.webhook.issues.opened', 'github.webhook.issues.opened.v1',
    issueEvent('example-org/settlement-service', 118, 'opened', 'Retries give up during bank maintenance', 'Settlement retries stop after three attempts while the bank API is in its weekend maintenance window.'))
  publish('evt.github.example-org.webhook.issues.opened', 'github.webhook.issues.opened.v1',
    issueEvent('example-org/billing-ui', 7, 'opened', 'Dark mode contrast', 'Not for this space.'))
  publish('evt.github.example-org.webhook.issues.labeled', 'github.webhook.issues.labeled.v1',
    issueEvent('example-org/settlement-service', 118, 'labeled', 'Retries give up during bank maintenance', 'Labelled priority/high.'))
  publish('evt.ops.alert.production', 'ops.alert.v1', { name: 'settlement_lag', severity: 'critical', summary: 'Settlement lag above 10 minutes.' })
  publish('evt.ops.alert.production', 'ops.alert.v1', { name: 'refund_errors', summary: 'Missing severity: this one is malformed.' })
  await expect.poll(async () => (await api.call<{ items: unknown[] }>('GET', `/spaces/${payments.id}/subscriptions/${issues.id}/deliveries`)).items.length, { timeout: 30_000 }).toBe(3)
  await expect.poll(async () => (await api.call<{ quarantined: number }>('GET', `/spaces/${payments.id}/subscriptions/${alerts.id}`)).quarantined, { timeout: 30_000 }).toBe(1)

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
  await capture(page, 'queue', async () => {
    await page.goto(`/spaces/${payments.id}/queue`)
    await expect(page.getByText('Refund export for finance').first()).toBeVisible()
  })
  await capture(page, 'queue-task', async () => {
    await page.goto(`/spaces/${payments.id}/queue`)
    await page.getByRole('listitem', { name: /Load-test the new retry budget/ }).click()
    await expect(page.getByText('Not eligible now')).toBeVisible()
  })
  await capture(page, 'accounts', async () => {
    await page.goto('/accounts')
    await expect(page.getByText('Team subscription (claude setup-token)')).toBeVisible()
  })
  await capture(page, 'space-accounts', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await page.getByRole('button', { name: 'Choose accounts' }).click()
    await expect(page.getByLabel('Allow team-claude')).toBeChecked()
  })
  await capture(page, 'messages', async () => {
    await page.goto(`/spaces/${payments.id}/messages`)
    await page.getByRole('listitem', { name: /Hold deploys until the bank window closes/ }).click()
    await expect(page.getByText('Not yet seen by a recipient')).toBeVisible()
  })
  await capture(page, 'human-global', async () => {
    await page.goto('/human')
    await expect(page.getByText('Review the wording of the refunds API reference')).toBeVisible()
  })
  await capture(page, 'human-space', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByLabel('Show resolved, withdrawn and expired').check()
    await expect(page.getByText('Which currency should the refund export use?')).toBeVisible()
  })
  await capture(page, 'human-form-default', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByRole('listitem', { name: /Deploy the settlement retry change/ }).click()
    await expect(page.getByText('Advisory decision')).toBeVisible()
  })
  await capture(page, 'events-catalog', async () => {
    await page.goto('/events')
    await expect(page.getByText('GitHub pull requests')).toBeVisible()
  })
  await capture(page, 'event-template', async () => {
    await page.goto('/events')
    await page.getByRole('link', { name: 'GitHub issues' }).click()
    await page.getByRole('button', { name: 'Test' }).click()
    await expect(page.getByText('accepted', { exact: true })).toBeVisible()
  })
  await capture(page, 'events-subscribe', async () => {
    await page.goto(`/spaces/${docs.id}/events`)
    await page.getByRole('button', { name: 'Subscribe' }).click()
    await page.getByLabel('Name').fill('docs pull requests')
    await page.getByRole('textbox', { name: 'Connection' }).click()
    await page.getByRole('option', { name: 'pulse-prod' }).click()
    await page.getByRole('textbox', { name: 'Template' }).click()
    await page.getByRole('option', { name: 'GitHub pull requests · v1' }).click()
    await page.getByLabel('account_key').fill('example-org')
    await page.getByLabel('repository').fill('example-org/docs')
  })
  await capture(page, 'events-subscription', async () => {
    await page.goto(`/spaces/${payments.id}/events/${issues.id}`)
    await expect(page.getByText('filtered', { exact: true })).toBeVisible()
  })
  await capture(page, 'events-quarantine', async () => {
    await page.goto(`/spaces/${payments.id}/events/${alerts.id}`)
    await page.getByRole('tab', { name: /Quarantine/ }).click()
    await expect(page.getByRole('button', { name: 'Reprocess' })).toBeVisible()
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
  mkdirSync(join(process.cwd(), 'test-results'), { recursive: true })
  writeFileSync(join(process.cwd(), 'test-results', 'a11y-report.txt'), a11y.join('\n') + '\n')
  expect(a11y, `accessibility violations:\n${a11y.join('\n')}`).toEqual([])
})
