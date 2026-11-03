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
  test.setTimeout(900_000) // one journey captures every page in both themes
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

  type Space = { id: string; version: number; workdir: string }
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

  // Catalog: an engineering base prompt and a control prompt that extends it (two versions), a reviewer prompt; profiles;
  // role templates; and the payments space's roles. Control is then launched by its role alone.
  type P = { id: string; versions: { id: string; state: string; version: number; number: number }[] }
  const prompt = async (slug: string, name: string, body: string) => api.call<P>('POST', '/prompts', { slug, name, kind: 'role', body })
  const publishPrompt = async (id: string) => {
    const p = await api.call<P>('GET', `/prompts/${id}`)
    const d = p.versions.find((v) => v.state === 'draft')!
    return api.call<{ id: string; number: number }>('POST', `/prompts/${id}/publish`, undefined, { 'If-Match': `"${d.version}"` })
  }
  // Updates the current draft (with its version) or starts a new one.
  const redraft = async (id: string, body: string, extendsId = '', notes = '') => {
    const d = (await api.call<P>('GET', `/prompts/${id}`)).versions.find((v) => v.state === 'draft')
    return api.call('PUT', `/prompts/${id}/draft`, { body, extends: extendsId || undefined, notes }, d ? { 'If-Match': `"${d.version}"` } : {})
  }
  const basePrompt = await prompt('engineering-base', 'Engineering base', '## How we work\n\n- Small pull requests, one concern each.\n- Tests before the fix.\n- Never push to main; open a pull request.')
  const baseV1 = await publishPrompt(basePrompt.id)
  const ctl = await prompt('control', 'Control', '')
  await redraft(ctl.id, '## Your role\n\nYou coordinate this space: read the inbox, keep the queue in order, start one worker per eligible task, and review results before marking work done.', baseV1.id)
  await publishPrompt(ctl.id)
  await redraft(ctl.id, '## Your role\n\nYou coordinate this space: read the inbox, keep the queue in order, start one worker per eligible task, and review results before marking work done.\n\nAsk a person before anything that touches production.', baseV1.id, 'Ask before production changes')
  await publishPrompt(ctl.id)
  const rev = await prompt('reviewer', 'Reviewer', 'Review open pull requests for correctness, tests and refund idempotency. Summarise risky changes first.')
  await publishPrompt(rev.id)
  type Prof = { id: string }
  const sonnet = await api.call<Prof>('PUT', '/harness-profiles/new', { slug: 'claude-default', name: 'Claude Code (default model)', harness: 'claude-code', model: '',
    settings: { permission_mode: 'acceptEdits' }, description: 'Interactive Claude Code; edits accepted, commands asked.' })
  const codexProf = await api.call<Prof>('PUT', '/harness-profiles/new', { slug: 'codex-default', name: 'Codex (default model)', harness: 'codex', model: '', settings: {} })
  // Skills: the built-in HallPass skill and a custom runbook, both given to control.
  type Sk = { id: string; slug: string; versions: { state: string; version: number }[] }
  const hallpass = (await api.call<{ items: Sk[] }>('GET', '/skills')).items.find((x) => x.slug === 'hallpass')!
  const runbook = await api.call<Sk>('POST', '/skills', { slug: 'refunds-runbook', name: 'Refunds runbook', description: 'How to investigate a stuck refund.',
    files: { 'SKILL.md': '---\nname: refunds-runbook\ndescription: Use when a refund is stuck in pending or a customer reports a missing refund.\n---\n\n# Refunds runbook\n\n1. Find the refund in the settlement log by its idempotency key.\n2. Check the bank window before retrying; never retry inside it.\n3. Record what you found in memory/refunds.md.\n',
      'references/queries.md': '# Useful queries\n\n- Pending refunds older than a day: `select id from refunds where state = \'pending\' and created_at < now() - interval \'1 day\'`\n' } })
  await api.call('POST', `/skills/${runbook.id}/publish`, undefined, { 'If-Match': `"${runbook.versions.find((v) => v.state === 'draft')!.version}"` })
  const tControl = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: 'control', name: 'Control', kind: 'control', profile_id: sonnet.id, prompt_id: ctl.id,
    memory_conventions: 'Keep decisions in memory/decisions.md with absolute dates.', settings: {}, skills: [{ skill_id: hallpass.id }, { skill_id: runbook.id }] })
  const tReviewer = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: 'reviewer', name: 'Reviewer', kind: 'worker', profile_id: codexProf.id, prompt_id: rev.id, settings: {} })
  await api.call('PUT', `/spaces/${payments.id}/roles/control`, { template_id: tControl.id, instructions: 'Settlement services live in services/settlement.', settings: {} })
  await api.call('PUT', `/spaces/${payments.id}/roles/reviewer`, { template_id: tReviewer.id, settings: {} })

  // MCP gateway: the dev stack's fake ticket tracker, granted to control (every tool) and reviewer (read-only tools).
  const mcpUrl = readFileSync(env('SM_MCP_URL_FILE'), 'utf8').trim()
  const tickets = await api.call<{ id: string }>('POST', '/mcp-servers', { slug: 'tickets', name: 'Ticket tracker', url: mcpUrl, allow_private: true,
    description: 'The payments team tracker (development fake).', headers: { Authorization: 'Bearer dev-mcp-token' } })
  await api.call('POST', `/mcp-servers/${tickets.id}/grants`, { space_id: payments.id, role: 'control' })
  await api.call('POST', `/mcp-servers/${tickets.id}/grants`, { space_id: payments.id, role: 'reviewer', tools: ['ticket_get', 'ticket_search'] })
  // Commenting on tickets needs a person's approval in payments; control asks for one comment at launch (the stub harness
  // makes the call through the bridge and calls again once it is approved).
  await api.call('POST', '/approval-policies', { space_id: payments.id, operation: 'mcp.tickets.ticket_comment', uses: 1, valid_seconds: 3600,
    note: 'Comments are visible to the finance team.' })

  // A knowledge base: a Git repository in the payments folder (knowledge on main, not checked out), sessions running as
  // a kb role; the stub harness answers questions as a KB session would.
  const kbRepo = join(env('SM_DIR'), 'agent', 'spaces', payments.workdir, 'repos', 'payments-kb')
  mkdirSync(join(kbRepo, 'decisions'), { recursive: true })
  writeFileSync(join(kbRepo, 'README.md'), '# Payments knowledge base\n\nDecisions, settlement rules and runbooks for the payments platform.\n')
  writeFileSync(join(kbRepo, 'decisions', 'ADR-0002-retry-budget.md'), '# ADR-0002: Retry budget per batch\n\nStatus: accepted\n\nSettlement retries use a budget of 5 per batch.\n')
  const gitEnv = { ...process.env, GIT_AUTHOR_NAME: 'Alex Morgan', GIT_AUTHOR_EMAIL: 'alex@example.com', GIT_COMMITTER_NAME: 'Alex Morgan', GIT_COMMITTER_EMAIL: 'alex@example.com' }
  for (const args of [['init', '-q', '-b', 'trunk'], ['add', '-A'], ['commit', '-q', '-m', 'Start the payments KB'], ['branch', 'main']])
    execFileSync('git', ['-C', kbRepo, ...args], { env: gitEnv })
  const tKB = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: 'kb-keeper', name: 'KB support', kind: 'kb', profile_id: sonnet.id, settings: {} })
  await api.call('PUT', `/spaces/${payments.id}/roles/kb-support`, { template_id: tKB.id, settings: {} })
  const kbase = await api.call<{ id: string }>('POST', `/spaces/${payments.id}/kbs`, { slug: 'payments', name: 'Payments knowledge', repo: 'repos/payments-kb',
    base_branch: 'main', space_role: 'kb-support', write_policy: 'review', max_sessions: 2, validator: ['sh', '-c', 'test -f README.md'],
    description: 'Decisions, settlement rules and runbooks.',
    entry_instructions: 'Read README.md first. You are a consumer of this KB: answer from it and propose changes; never accept them.\n' +
      '/kb-answer Settlement retries use a **budget of 5 per batch** (decisions/ADR-0002-retry-budget.md, accepted). Never retry inside the bank window.' })

  type Run = { id: string }
  const launch = (space: string, kind: string, role: string, harness: string, prompt: string) =>
    api.call<Run>('POST', `/spaces/${space}/runs`, { kind, role, harness, prompt })
  const control = await api.call<Run>('POST', `/spaces/${payments.id}/runs`, { role: 'control',
    prompt: 'Coordinate the retry-budget change across the settlement services.\n' +
      '/call spaceman_kb_ask {"kb":"payments","kind":"decision","question":"What retry budget did we decide for settlement, and where is it recorded?"}\n' +
      '/call tickets__ticket_comment {"id":"PAY-409","body":"Retry budget raised to 5 for weekend settlement."}' })
  const reviewer = await launch(payments.id, 'worker', 'reviewer', 'codex', 'Review the open pull requests and summarise the risky ones.')
  const finished = await launch(payments.id, 'worker', 'implementor-backend', 'claude-code', 'Add the retry budget to the settlement client.\n/transcript\n' +
    '/call spaceman_kb_ask {"kb":"payments","kind":"context","question":"Are there rules about when settlement may retry?"}\n' +
    '/call spaceman_human_ask {"type":"question","title":"Is a retry budget of 5 per batch enough for month-end settlement?"}\n' +
    '/write decisions.md # Decisions\\n\\n- 2026-11-02: settlement retries use a budget of 5 per batch, not per item.\\n- 2026-11-02: never retry inside the bank window (01:00-03:00 UTC).\\n\n' +
    '/write notes/settlement-client.md # Settlement client\\n\\nRetries live in `RetryBudget` (services/settlement/client/retry.go). The budget resets per batch.\\n')
  // Cross-space (ADR-0038): payments lets the docs site discover it and send questions and delegated work (replies as
  // summaries); the docs site lets its control send them. The docs site's control asks once it starts.
  const noInbound = { discover: false, questions: false, delegation: false, knowledge: false, kbs: [], disclosure: 'none', max_per_hour: 20, max_pending: 5 }
  await api.call('PUT', `/spaces/${payments.id}/xspace/settings`, { profile: 'Card processing, settlement and refunds. Ask about retry budgets, bank windows and the settlement API.',
    max_depth: 3, max_outgoing_per_hour: 60 })
  await api.call('PUT', `/spaces/${payments.id}/xspace/peers/${docs.id}`, { inbound: { ...noInbound, discover: true, questions: true, delegation: true, knowledge: true,
    kbs: ['payments'], disclosure: 'summary', max_per_hour: 10, max_pending: 3 }, outbound: { questions: true, delegation: false, knowledge: false } })
  await api.call('PUT', `/spaces/${docs.id}/xspace/peers/${payments.id}`, { inbound: { ...noInbound, questions: true },
    outbound: { questions: true, delegation: true, knowledge: true } })
  await launch(docs.id, 'control', 'control', 'claude-code', 'Keep the guides in step with the UI.\n' +
    '/call spaceman_xspace_request {"to":"payments","kind":"question","title":"Which retry budget should the settlement guide document?","body":"The settlement guide still says 3 retries per item. What is the current budget, and where is it decided?","deadline_minutes":1440}\n' +
    '/call spaceman_xspace_request {"to":"payments","kind":"delegation","title":"Add a runnable refund example to the API reference","body":"The refunds page has no example a reader can run against the sandbox. Could your team add one?","deadline_minutes":4320}')
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

  // Human-action policies: payments reminds about to-dos after a minute (so Needs you shows one reminded), and approvals
  // are reminded, escalated and routed to reviewers when the requester is gone.
  const policy = (type: string, p: Record<string, unknown>) => api.call('PUT', `/spaces/${payments.id}/human-policies/${type}`, {
    remind_after_minutes: 0, remind_every_minutes: 0, max_reminders: 1, escalate_after_minutes: 0, escalate_priority: '', escalate_assignee: '',
    requester_ended: 'reroute', hold_minutes: 60, successor_role: '', wake_idle: true, rate_per_run_hour: 30, rate_per_space_hour: 300, ...p })
  await policy('todo', { remind_after_minutes: 1 })
  await policy('approval', { remind_after_minutes: 30, remind_every_minutes: 120, max_reminders: 3, escalate_after_minutes: 240, escalate_priority: 'urgent',
    successor_role: 'reviewer', rate_per_run_hour: 10 })

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
  // A custom type: a deploy window request with a schema-driven form, enabled in payments as a variant.
  const dw = await api.call<{ id: string; versions: { version: number; state: string }[] }>('POST', '/human-types', { slug: 'deploy-window', name: 'Deploy window',
    description: 'Agree on when a production deployment may happen.',
    request_schema: { type: 'object', required: ['environment', 'change'], properties: { environment: { type: 'string', enum: ['staging', 'production'], title: 'Environment' },
      change: { type: 'string', title: 'Change' }, rollback: { type: 'string', title: 'Rollback plan' } }, additionalProperties: false },
    response_schema: { type: 'object', required: ['window_start'], properties: { window_start: { type: 'string', format: 'date-time', title: 'Window starts' },
      duration_minutes: { type: 'integer', title: 'Duration (minutes)' }, conditions: { type: 'string', title: 'Conditions' } }, additionalProperties: false },
    ui: { order: ['window_start', 'duration_minutes', 'conditions'], fields: { conditions: { widget: 'textarea', help: 'Anything the deployer must check first' }, environment: { widget: 'radio' } } },
    outcomes: ['scheduled', 'declined'], default_priority: 'high', default_expiry_minutes: 1440 })
  await api.call('POST', `/human-types/${dw.id}/publish`, undefined, { 'If-Match': `"${dw.versions[0].version}"` })
  await api.call('PUT', `/spaces/${payments.id}/human-types/${dw.id}`, { label: 'Production deploy window' })
  await human(payments.id, { type: 'custom', type_slug: 'deploy-window', title: 'Deploy payments-api v2.14.0',
    request: { environment: 'production', change: 'Time-based settlement retry budget', rollback: 'Redeploy v2.13.4' } })
  const answered = await human(payments.id, { type: 'question', title: 'Which currency should the refund export use?', request: { options: ['EUR', 'Original currency'] } })
  await api.call('POST', `/spaces/${payments.id}/human-actions/${answered.id}/respond`, { outcome: 'answered', answer: 'Original currency' }, { 'If-Match': `"${answered.version}"` })
  // The stopped worker asked a question before it ended: its answer goes to control, with where it came from.
  type HA = { id: string; version: number; title: string; requester_run?: string }
  const workerAsked = async () => (await api.call<{ items: HA[] }>('GET', `/spaces/${payments.id}/human-actions`)).items.find((a) => a.requester_run === finished.id)
  await expect.poll(async () => (await workerAsked())?.id ?? '', { timeout: 30_000 }).not.toBe('')
  const asked = (await workerAsked())!
  await api.call('POST', `/spaces/${payments.id}/human-actions/${asked.id}/respond`, { outcome: 'answered', answer: 'Yes for now; revisit after the December close.' },
    { 'If-Match': `"${asked.version}"` })

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
    await expect(page.getByRole('cell', { name: 'reviewer (worker)' })).toBeVisible()
  })
  // Relocation dialog (SM-SPACE-005). The second node is enrolled through the API only (no second agent runs in this
  // stack), so it has never connected: the dialog names it as not available instead of offering it.
  const spare = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'build-02', description: 'Second workstation' })
  const enrolled = await page.request.post(`${base}/agent/v1/enroll`, { data: { token: spare.token } })
  expect(enrolled.ok()).toBeTruthy()
  await capture(page, 'space-relocate', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await page.getByRole('button', { name: 'Relocate' }).click()
    await expect(page.getByText('Not available as a destination: build-02 (never connected)')).toBeVisible()
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
    await page.getByRole('textbox', { name: 'Space role' }).click()
    await page.getByRole('option', { name: 'reviewer' }).click()
    await page.getByLabel('Initial prompt').fill('Review the open refund pull requests; flag anything that is not idempotent.')
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
  await capture(page, 'human-types', async () => {
    await page.goto('/human-types')
    await page.getByRole('cell', { name: /Deploy window/ }).first().click()
    await expect(page.getByText('Preview of the request form')).toBeVisible()
  })
  await capture(page, 'human-form-custom', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByRole('listitem', { name: /Deploy payments-api v2.14.0/ }).click()
    await expect(page.getByText('Window starts')).toBeVisible()
  })
  await capture(page, 'messages', async () => {
    await page.goto(`/spaces/${payments.id}/messages`)
    await page.getByRole('listitem', { name: /Hold deploys until the bank window closes/ }).click()
    await expect(page.getByText('Not yet seen by a recipient')).toBeVisible()
  })
  await capture(page, 'prompts', async () => {
    await page.goto('/prompts')
    await expect(page.getByText('Engineering base')).toBeVisible()
  })
  await capture(page, 'prompt-detail', async () => {
    await page.goto(`/prompts/${ctl.id}`)
    await expect(page.getByText('Version 2 · Ask before production changes')).toBeVisible()
  })
  await capture(page, 'prompts-diff', async () => {
    await page.goto(`/prompts/${ctl.id}`)
    await page.getByRole('button', { name: 'Diff' }).first().click()
    await expect(page.getByLabel('Diff')).toContainText('Ask a person before anything that touches production.')
  })
  await capture(page, 'roles', async () => {
    await page.goto('/roles')
    await expect(page.getByRole('cell', { name: /Reviewer/ }).first()).toBeVisible()
  })
  await capture(page, 'skills', async () => {
    await page.goto('/skills')
    await expect(page.getByText('Refunds runbook')).toBeVisible()
  })
  await capture(page, 'skill-detail', async () => {
    await page.goto('/skills')
    await page.getByText('HallPass gateway').click()
    await expect(page.getByRole('tab', { name: 'references/commands.md' })).toBeVisible()
  })
  await capture(page, 'mcp-servers', async () => {
    await page.goto('/mcp-servers')
    await expect(page.getByText('Ticket tracker')).toBeVisible()
  })
  await capture(page, 'mcp-server-detail', async () => {
    await page.goto('/mcp-servers')
    await page.getByText('Ticket tracker').click()
    await page.getByRole('button', { name: 'Check connection' }).click()
    await expect(page.getByText('tickets__ticket_search')).toBeVisible()
  })
  type Apv = { id: string; state: string; human_action_id: string; executions: { state: string }[] }
  const approvalsOf = async () => (await api.call<{ items: Apv[] }>('GET', `/spaces/${payments.id}/approvals`)).items
  await expect.poll(async () => (await approvalsOf()).length, { timeout: 30_000 }).toBe(1)
  const apv = (await approvalsOf())[0]
  await capture(page, 'approval-policies', async () => {
    await page.goto('/approval-policies')
    await expect(page.getByText('mcp.tickets.ticket_comment')).toBeVisible()
  })
  await capture(page, 'approval-enforced', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByRole('listitem', { name: /Approve mcp\.tickets\.ticket_comment/ }).click()
    await expect(page.getByText('Enforced approval').first()).toBeVisible()
    await expect(page.getByText('Not executed.')).toBeVisible()
  })
  const pendingAct = await api.call<{ id: string; version: number }>('GET', `/spaces/${payments.id}/human-actions/${apv.human_action_id}`)
  await api.call('POST', `/spaces/${payments.id}/human-actions/${pendingAct.id}/respond`, { outcome: 'approved' }, { 'If-Match': `"${pendingAct.version}"` })
  await expect.poll(async () => (await approvalsOf())[0].executions.map((x) => x.state).join(','), { timeout: 30_000 }).toBe('succeeded')
  await capture(page, 'approval-executed', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByLabel('Show resolved, withdrawn and expired').check()
    await page.getByRole('listitem', { name: /Approve mcp\.tickets\.ticket_comment/ }).click()
    await expect(page.getByText('succeeded')).toBeVisible()
  })
  // The worker's memory was checkpointed when it stopped and synced to central.
  await expect.poll(async () => {
    const m = await api.call<{ items: { role: string; head?: string }[] }>('GET', `/spaces/${payments.id}/memory`)
    return m.items.find((c) => c.role === 'implementor-backend')?.head ?? ''
  }, { timeout: 30_000 }).not.toBe('')
  // The dreamer: a dreamer role using the built-in dream prompt, and a nightly dream started now. The stub reports
  // what it found the way a dreamer would.
  const prompts = await api.call<{ items: { id: string; slug: string }[] }>('GET', '/prompts')
  const dreamPrompt = prompts.items.find((p) => p.slug === 'dream-consolidation')!
  const tDream = await api.call<{ id: string }>('PUT', '/role-templates/new', { slug: 'dreamer', name: 'Dreamer', kind: 'dreamer', profile_id: sonnet.id,
    prompt_id: dreamPrompt.id, settings: {} })
  await api.call('PUT', `/spaces/${payments.id}/roles/dreamer`, { template_id: tDream.id, settings: {} })
  const nightly = await api.call<{ id: string }>('POST', `/spaces/${payments.id}/dreams`, { slug: 'nightly', name: 'Nightly consolidation', space_role: 'dreamer',
    memory_roles: ['implementor-backend', 'control'], interval_hours: 24, max_minutes: 20, max_files: 20,
    instructions: 'Focus on settlement decisions.\n/call spaceman_dream_report {"summary":"Read the new implementor-backend transcript segment and both memories. ' +
      'The retry budget and the bank window are already recorded in decisions.md; nothing needed changing.","no_change":true,' +
      '"findings":[{"kind":"insight","summary":"Retries and the bank window are documented in two places; consider linking them from the README."}]}' })
  await api.call('POST', `/spaces/${payments.id}/dreams/${nightly.id}/start`)
  await expect.poll(async () => {
    const l = await api.call<{ items: { id: string; last_run?: { state: string } }[] }>('GET', `/spaces/${payments.id}/dreams`)
    return l.items.find((d) => d.id === nightly.id)?.last_run?.state ?? ''
  }, { timeout: 60_000 }).toBe('succeeded')
  await capture(page, 'space-memory', async () => {
    await page.goto(`/spaces/${payments.id}/memory`)
    await expect(page.getByText('in sync').first()).toBeVisible({ timeout: 20_000 })
  })
  await capture(page, 'dream-runs', async () => {
    await page.goto(`/spaces/${payments.id}/memory`)
    await page.getByRole('button', { name: 'Nightly consolidation' }).click()
    await expect(page.getByText('consider linking them from the README')).toBeVisible()
  })
  await capture(page, 'memory-files', async () => {
    await page.goto(`/spaces/${payments.id}/memory`)
    await page.getByRole('cell', { name: 'implementor-backend' }).click()
    await page.getByRole('button', { name: 'decisions.md' }).click()
    await expect(page.getByText('never retry inside the bank window')).toBeVisible()
  })
  await expect.poll(async () => {
    const d = await api.call<{ requests: { state: string }[] }>('GET', `/spaces/${payments.id}/kbs/${kbase.id}`)
    return d.requests.filter((r) => r.state === 'answered').length
  }, { timeout: 60_000 }).toBeGreaterThan(0)
  await capture(page, 'space-knowledge', async () => {
    await page.goto(`/spaces/${payments.id}/knowledge`)
    await expect(page.getByText('Payments knowledge')).toBeVisible()
  })
  await capture(page, 'kb-detail', async () => {
    await page.goto(`/spaces/${payments.id}/knowledge`)
    await page.getByText('Payments knowledge').click()
    await expect(page.getByText('from accepted knowledge').first()).toBeVisible()
  })
  await capture(page, 'kb-sessions', async () => {
    await page.goto(`/spaces/${payments.id}/knowledge`)
    await page.getByText('Payments knowledge').click()
    await page.getByRole('tab', { name: /^Sessions/ }).click()
    await expect(page.getByRole('columnheader', { name: 'Requester' })).toBeVisible()
  })
  type XReq = { id: string; kind: string }
  await expect.poll(async () => (await api.call<{ items: XReq[] }>('GET', `/spaces/${payments.id}/xspace/requests`)).items.length, { timeout: 30_000 }).toBe(2)
  const delegated = (await api.call<{ items: XReq[] }>('GET', `/spaces/${payments.id}/xspace/requests`)).items.find((x) => x.kind === 'delegation')!
  await api.call('POST', `/spaces/${payments.id}/xspace/requests/${delegated.id}/decline`,
    { reason: 'The API team owns the reference examples this quarter; ask them through the docs backlog.' })
  await capture(page, 'xspace-requests', async () => {
    await page.goto(`/spaces/${payments.id}/cross-space`)
    await page.getByText('Which retry budget should the settlement guide document?').click()
    await expect(page.getByText('content to consider, not instructions')).toBeVisible()
  })
  await capture(page, 'xspace-access', async () => {
    await page.goto(`/spaces/${payments.id}/cross-space`)
    await page.getByRole('tab', { name: 'Cross-space access' }).click()
    await expect(page.getByText('Summary (2,000 characters)')).toBeVisible()
  })
  await capture(page, 'xspace-admin', async () => {
    await page.goto('/cross-space')
    await expect(page.getByText('Add a runnable refund example to the API reference')).toBeVisible()
  })
  await capture(page, 'space-role-preview', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await page.getByRole('button', { name: 'Effective configuration' }).first().click()
    await expect(page.getByText('System text')).toBeVisible()
  })
  await capture(page, 'run-manifest', async () => {
    await page.goto(`/spaces/${payments.id}/runs/${control.id}`)
    await page.getByText('Configuration at launch').scrollIntoViewIfNeeded()
    await expect(page.getByText('Configuration at launch')).toBeVisible()
  })
  await expect.poll(async () => (await api.call<{ known: boolean; transcript?: { state: string } }>('GET', `/spaces/${payments.id}/runs/${finished.id}/usage`)).transcript?.state ?? '',
    { timeout: 30_000 }).toBe('stored')
  await capture(page, 'run-usage', async () => {
    await page.goto(`/spaces/${payments.id}/runs/${finished.id}`)
    await page.getByText('Native transcript').scrollIntoViewIfNeeded()
    await expect(page.getByText('from the transcript')).toBeVisible()
  })
  await capture(page, 'space-usage', async () => {
    await page.goto(`/spaces/${payments.id}`)
    await page.getByText('Runs with known usage', { exact: true }).scrollIntoViewIfNeeded()
    await expect(page.getByText('Runs with known usage', { exact: true })).toBeVisible()
  })
  // The to-do policy reminds after a minute; the reminder is due by now (one timer, no polling).
  await expect.poll(async () => (await api.call<{ items: { title: string; reminder_count: number }[] }>('GET', `/spaces/${payments.id}/human-actions?pending=true`))
    .items.find((a) => a.title === "Confirm the bank's maintenance window")?.reminder_count ?? 0, { timeout: 120_000 }).toBeGreaterThan(0)
  await capture(page, 'human-global', async () => {
    await page.goto('/human')
    await expect(page.getByText('Review the wording of the refunds API reference')).toBeVisible()
    await expect(page.getByText('reminded', { exact: true }).first()).toBeVisible()
  })
  await capture(page, 'human-policies', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByRole('button', { name: 'Policies' }).click()
    await expect(page.getByText('this space · approval')).toBeVisible()
  })
  await capture(page, 'human-rerouted', async () => {
    await page.goto(`/spaces/${payments.id}/human`)
    await page.getByLabel('Show resolved, withdrawn and expired').check()
    await page.getByRole('listitem', { name: /Is a retry budget of 5 per batch enough/ }).click()
    await expect(page.getByText('rerouted', { exact: true })).toBeVisible()
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
  await capture(page, 'events-publishing', async () => {
    await page.goto('/events')
    await page.getByRole('tab', { name: 'Publishing' }).click()
    await expect(page.getByText('publishing', { exact: true })).toBeVisible({ timeout: 30_000 })
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
  // The same form for a source without JetStream: core NATS delivery, labelled non-durable, with no start position.
  await capture(page, 'events-subscribe-core', async () => {
    await page.goto(`/spaces/${docs.id}/events`)
    await page.getByRole('button', { name: 'Subscribe' }).click()
    await page.getByLabel('Name').fill('docs build pings')
    await page.getByRole('textbox', { name: 'Connection' }).click()
    await page.getByRole('option', { name: 'pulse-prod' }).click()
    await page.getByRole('textbox', { name: 'Template' }).click()
    await page.getByRole('option', { name: 'GitHub pull requests · v1' }).click()
    await page.getByLabel('account_key').fill('example-org')
    await page.getByLabel('repository').fill('example-org/docs')
    await page.getByText('Core NATS (non-durable)').click()
    await expect(page.getByText('Non-durable subscription', { exact: true })).toBeVisible()
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
  // Replay (SM-EVT-015): an explicit replay of the issues subscription; its deliveries show origin replay beside the live ones.
  type Sub = { version: number }
  const cur = await api.call<Sub>('GET', `/spaces/${payments.id}/subscriptions/${issues.id}`)
  await api.call('POST', `/spaces/${payments.id}/subscriptions/${issues.id}/replay`, { policy: 'all', reason: 'Template v2 adds labels; re-check the backlog' },
    { 'If-Match': `"${cur.version}"` })
  await expect.poll(async () => (await api.call<{ items: { origin: string }[] }>('GET', `/spaces/${payments.id}/subscriptions/${issues.id}/deliveries`)).items
    .filter((d) => d.origin === 'replay').length, { timeout: 30_000 }).toBeGreaterThan(0)
  await capture(page, 'event-replay', async () => {
    await page.goto(`/spaces/${payments.id}/events/${issues.id}`)
    await expect(page.getByText('Replay', { exact: true }).first()).toBeVisible()
  })
  // Core NATS (ADR-0042): a non-durable subscription for a source that publishes without JetStream. Its page carries the
  // non-durable badge and explanation, and has no cursor.
  const heartbeats = await api.call<{ id: string }>('POST', `/spaces/${payments.id}/subscriptions`, { name: 'ledger heartbeats', connection_id: conn.id,
    delivery: 'core', custom: { subject: 'evt.ops.heartbeat.ledger', variables: [], filters: [], title: 'Ledger heartbeat: {{data.status}}',
      body: 'The ledger service reported **{{data.status}}**.' } })
  await expect.poll(async () => (await api.call<{ state: string }>('GET', `/spaces/${payments.id}/subscriptions/${heartbeats.id}`)).state, { timeout: 30_000 }).toBe('running')
  publish('evt.ops.heartbeat.ledger', 'ops.heartbeat.v1', { status: 'degraded' })
  await expect.poll(async () => (await api.call<{ items: unknown[] }>('GET', `/spaces/${payments.id}/subscriptions/${heartbeats.id}/deliveries`)).items.length,
    { timeout: 30_000 }).toBe(1)
  await capture(page, 'events-core-subscription', async () => {
    await page.goto(`/spaces/${payments.id}/events/${heartbeats.id}`)
    await expect(page.getByText('Non-durable subscription (core NATS)')).toBeVisible()
  })

  // Limits (SM-LIMIT-001): overrides with a token budget; Schedules (SM-LIMIT-003): a weekday review in Vilnius time and
  // an hourly task.
  await api.call('PUT', `/spaces/${payments.id}/limits`, { values: { max_concurrent_runs: 4, max_tool_calls_per_run: 2000, budget_tokens_24h: 5_000_000,
    budget_cost_cents_24h: 5000, task_deadline_minutes: 240 }, on_lower: 'drain' })
  await capture(page, 'space-limits', async () => {
    await page.goto(`/spaces/${payments.id}/limits`)
    await expect(page.getByText('Not a hard currency limit', { exact: true })).toBeVisible()
  })
  await api.call('POST', `/spaces/${payments.id}/schedules`, { name: 'weekday review', description: 'Morning look at the queue and open incidents', kind: 'cron',
    cron: '0 9 * * 1-5', timezone: 'Europe/Vilnius', missed: 'run_once', target: 'inbox', title: 'Morning review',
    body: 'Review the queue, open incidents and anything waiting for people.' })
  await api.call('POST', `/spaces/${payments.id}/schedules`, { name: 'reconcile ledgers', kind: 'interval', interval_seconds: 3600, missed: 'catch_up', catch_up_max: 3,
    target: 'task', title: 'Reconcile settlement ledgers', priority: 'high', assignee_role: 'implementor-backend' })
  await capture(page, 'space-schedules', async () => {
    await page.goto(`/spaces/${payments.id}/schedules`)
    await expect(page.getByText('reconcile ledgers')).toBeVisible()
  })

  // Repositories (ADR-0044): a local bare repository stands in for the settlement service's remote (development mode
  // allows file URLs). The node clones it, a worker gets a worktree with an unpushed commit and an untracked file, and a
  // sync reports it all; the knowledge base's folder is listed too (role kb, manual).
  const remote = join(env('SM_DIR'), 'remotes', 'settlement.git')
  const seedDir = join(env('SM_DIR'), 'remotes', 'settlement-seed')
  mkdirSync(seedDir, { recursive: true })
  execFileSync('git', ['init', '-q', '--bare', '-b', 'main', remote], { env: gitEnv })
  writeFileSync(join(seedDir, 'README.md'), '# Settlement service\n\nBatches card settlements and talks to the acquiring bank.\n')
  for (const args of [['init', '-q', '-b', 'main'], ['add', '-A'], ['commit', '-q', '-m', 'Settlement service skeleton'], ['remote', 'add', 'origin', remote], ['push', '-q', 'origin', 'main']])
    execFileSync('git', ['-C', seedDir, ...args], { env: gitEnv })
  await api.call('POST', `/spaces/${payments.id}/repositories`, { name: 'settlement', path: 'repos/settlement', url: `file://${remote}`,
    default_branch: 'main', description: 'Settlement batching service' })
  type Repos = { items: { name: string; status?: { state: string; worktrees: unknown[]; untracked: number } }[]; sync_pending: boolean }
  const repoState = async (name: string) => (await api.call<Repos>('GET', `/spaces/${payments.id}/repositories`)).items.find((r) => r.name === name)?.status
  await expect.poll(async () => (await repoState('settlement'))?.state, { timeout: 30_000 }).toBe('present')
  const wtRun = await api.call<Run>('POST', `/spaces/${payments.id}/runs`, { kind: 'worker', role: 'implementor-backend', harness: 'claude-code',
    prompt: 'Add the retry budget to the settlement batcher.', worktree: { repo: 'settlement', branch: 'spaceman/retry-budget' } })
  await expect.poll(() => running(payments.id, wtRun.id), { timeout: 30_000 }).toBe('running')
  const wtDir = join(env('SM_DIR'), 'agent', 'spaces', payments.workdir, 'worktrees', 'spaceman-retry-budget')
  writeFileSync(join(wtDir, 'retry.go'), 'package settlement\n\n// RetryBudget is the number of retries per batch.\nconst RetryBudget = 5\n')
  for (const args of [['add', 'retry.go'], ['commit', '-q', '-m', 'Add the retry budget']]) execFileSync('git', ['-C', wtDir, ...args], { env: gitEnv })
  writeFileSync(join(wtDir, 'NOTES.md'), 'Check the bank window before merging.\n')
  await api.call('POST', `/spaces/${payments.id}/repositories/sync`, {})
  await expect.poll(async () => {
    const r = await api.call<Repos>('GET', `/spaces/${payments.id}/repositories`)
    const st = r.items.find((x) => x.name === 'settlement')?.status
    return !r.sync_pending && (st?.worktrees.length ?? 0) === 1 && st?.untracked === 0
  }, { timeout: 30_000 }).toBe(true)
  await capture(page, 'space-repositories', async () => {
    await page.goto(`/spaces/${payments.id}/repositories`)
    await expect(page.getByText('repos/settlement')).toBeVisible()
  })
  await capture(page, 'space-repository-detail', async () => {
    await page.goto(`/spaces/${payments.id}/repositories`)
    await page.getByText('repos/settlement').click()
    await expect(page.getByText('spaceman/retry-budget').first()).toBeVisible()
  })
  await capture(page, 'space-repository-form', async () => {
    await page.goto(`/spaces/${payments.id}/repositories`)
    await page.getByRole('button', { name: 'Add repository' }).click()
    await expect(page.getByLabel('Clone URL')).toBeVisible()
  })
  await capture(page, 'setup-node-enroll', async () => {
    await page.goto('/nodes')
    await expect(page.getByText('connected', { exact: true })).toBeVisible()
  })
  await capture(page, 'auth-users', async () => {
    await page.goto('/users')
    await expect(page.getByText('Sam Rivera')).toBeVisible()
  })
  // Sign-in providers (ADR-0029): a Porch provider (fictional issuer; discovery runs only at sign-in, so nothing is
  // contacted here) with group mappings, a disabled second provider, and Sam's linked Porch identity. Created with the
  // root token: provider changes otherwise need a fresh step-up.
  type Prov = { id: string }
  const porch = await asRoot.call<Prov>('POST', '/oidc/providers', { display_name: 'Porch', slug: 'porch', issuer: 'https://id.example.com',
    client_id: 'spaceman-acme', client_secret: `stub-${password()}`, scopes: ['openid', 'profile', 'email', 'groups'], groups_claim: 'porch.groups',
    provisioning: 'jit', jit: { groups: ['spaceman-users'], email_domains: [], tenants: [], hosted_domains: [] }, enabled: true })
  await asRoot.call('POST', '/oidc/providers', { display_name: 'Corporate Entra ID', slug: 'entra', issuer: 'https://login.example.net/tenant-a/v2.0',
    client_id: 'b1e0-spaceman', scopes: ['openid', 'profile', 'email'], groups_claim: 'groups', enabled: false })
  for (const [group_value, role, domain] of [['spaceman-admins', 'admin', '*'], ['spaceman-viewers', 'viewer', '*'], ['payments-oncall', 'operator', payments.id],
    ['docs-writers', 'operator', docs.id]] as const) {
    await asRoot.call('POST', `/oidc/providers/${porch.id}/mappings`, { group_value, role, domain })
  }
  const sam = (await api.call<{ items: { id: string; email?: string }[] }>('GET', '/users')).items.find((u) => u.email === 'sam@example.com')!
  await asRoot.call('POST', `/users/${sam.id}/identities`, { provider_id: porch.id, subject: 'u_01jcz8k4m2v7p3q9r5s6t8w0xa' })
  await capture(page, 'sign-in-providers', async () => {
    await page.goto('/sign-in-providers')
    await expect(page.getByText('payments-oncall')).toBeVisible()
  })
  await capture(page, 'sign-in-provider-form', async () => {
    await page.goto('/sign-in-providers')
    await page.getByRole('button', { name: 'Actions for Porch' }).click()
    await page.getByRole('menuitem', { name: 'Edit…' }).click()
    await expect(page.getByText('A secret is stored. Leave empty to keep it.')).toBeVisible()
  })
  {
    const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 } })
    const anon = await ctx.newPage()
    await anon.goto('/login')
    await capture(anon, 'sign-in-sso', async () => {
      await anon.goto('/login')
      await expect(anon.getByRole('link', { name: 'Sign in with Porch' })).toBeVisible()
    })
    await ctx.close()
  }
  await capture(page, 'auth-tokens', async () => {
    await page.goto('/tokens')
    await expect(page.getByText('terraform-ci').first()).toBeVisible()
  })
  await capture(page, 'audit-log', async () => {
    await page.goto('/audit')
    await expect(page.getByText('run.launched').first()).toBeVisible()
  })

  // Backups (SM-STORE-005): one manual backup of the seeded stack, verified.
  type Backups = { busy: boolean; items: { id: string; state: string; verify_state: string }[] }
  const made = await api.call<{ id: string }>('POST', '/system/backups')
  await expect.poll(async () => (await api.call<Backups>('GET', '/system/backups')).items.find((i) => i.id === made.id)?.state, { timeout: 60_000 }).toBe('succeeded')
  await expect.poll(async () => (await api.call<Backups>('GET', '/system/backups')).busy, { timeout: 30_000 }).toBe(false)
  await api.call('POST', `/system/backups/${made.id}/verify`)
  await expect.poll(async () => (await api.call<Backups>('GET', '/system/backups')).items.find((i) => i.id === made.id)?.verify_state, { timeout: 60_000 }).toBe('ok')
  await capture(page, 'ops-backups', async () => {
    await page.goto('/backups')
    await expect(page.getByText(made.id)).toBeVisible()
    await expect(page.getByText(/^verified/).first()).toBeVisible()
  })
  mkdirSync(join(process.cwd(), 'test-results'), { recursive: true })
  writeFileSync(join(process.cwd(), 'test-results', 'a11y-report.txt'), a11y.join('\n') + '\n')
  expect(a11y, `accessibility violations:\n${a11y.join('\n')}`).toEqual([])
})
