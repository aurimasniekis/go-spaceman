import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { createReadStream, existsSync, statSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { extname, join, normalize } from 'node:path'

/**
 * The Mini App's pages (TG-10-2) at a phone's width (390 px), in a real browser without a bot: the built `dist/tg.html` is
 * served locally, Telegram's loader is a stub and the API is mocked at the network. Each page is checked for sideways
 * overflow and with axe (WCAG 2.1 A/AA); the queue is reordered with touch pointer events; "New task" is offered to
 * operators only. Needs `pnpm build` output in web/dist. Run alone: `pnpm exec playwright test e2e/tg-pages.spec.ts`.
 */
const PORT = 39732
const dist = join(process.cwd(), 'dist')
const SPACE = 'spc_pages1'
const T = (n: number) => 'tsk_page' + n
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true })

let server: Server
test.beforeAll(async () => {
  expect(existsSync(join(dist, 'tg.html')), 'run `pnpm build` first').toBe(true)
  server = createServer((req, res) => {
    let file = join(dist, normalize(new URL(req.url ?? '/', 'http://x').pathname))
    if (!file.startsWith(dist) || !existsSync(file) || statSync(file).isDirectory()) file = join(dist, 'tg.html')
    const types: Record<string, string> = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.wasm': 'application/wasm', '.woff2': 'font/woff2' }
    res.setHeader('Content-Type', types[extname(file)] ?? 'application/octet-stream')
    createReadStream(file).pipe(res)
  })
  await new Promise<void>((r) => server.listen(PORT, '127.0.0.1', r))
})
test.afterAll(() => new Promise<void>((r) => server.close(() => r())))

const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
const now = '2026-11-14T10:00:00Z'
const task = (n: number, status = 'ready') => ({
  id: T(n), space_id: SPACE, title: `Task number ${n} with a rather long title that has to wrap on a phone`, status, priority: 'normal', kind: 'ordinary', position: n, depth: 0,
  depends_on: [], blockers: [], exclusions: [], version: 1, manual: false, preemptions: 0, objective: 'Do the **thing**.', acceptance: '', created_at: now, attachments: [],
})
const run = { id: 'run_pages1', space_id: SPACE, role: 'implementor', turn: 2, kind: 'worker', harness: 'claude-code', observed: 'running', desired: 'running', status: 'working', created_at: now, started_at: now, generation: 1, adopted: false, instance_id: 'i', node_id: 'n', launch: {} }
const message = { id: 'msg_pages1', space_id: SPACE, kind: 'request', subject: 'Please look at the deploy', body: 'Hello', state: 'available', from_kind: 'control', to_kind: 'operators', created_at: now, attachments: [] }

function basePrefs(): Record<string, unknown> {
  const d = { assigned_to_me: true, clarifications: true, digest: true, digest_time: '09:00', escalations: true, failed_runs: 'digest', inbox_items: 'digest', min_priority: 'normal', new_actions: true,
    operator_messages: true, quiet_hours: true, quiet_hours_end: '08:00', quiet_hours_start: '23:00', reminders: 'push', tasks_in_review: 'digest' }
  return { link_id: 'tgl_1', overrides: {}, defaults: d, effective: { ...d, muted_spaces: [] }, timezone: 'Europe/Vilnius', topics: { available: false }, version: 3 }
}

/** The fixtures a test can replace: the queue's tasks, the scheduled messages to control and the space's attention counts. */
type Fixtures = { prefs?: Record<string, unknown>; putStatus?: number; role?: string; path: string; tasks?: unknown[]; scheduled?: unknown[]; attention?: Record<string, unknown> }

async function launch(page: Page, o: Fixtures) {
  const moves: unknown[] = []
  const created: unknown[] = []
  const requests: string[] = []
  const puts: Array<{ ifMatch?: string; body: Record<string, unknown> }> = []
  let prefs = o.prefs ?? basePrefs()
  let conflicts = o.putStatus === 412 ? 1 : 0
  await page.route('https://telegram.org/js/telegram-web-app.js', (r) =>
    r.fulfill({
      contentType: 'text/javascript',
      body: `(() => { const b = () => ({ isVisible: false, show() {}, hide() {}, onClick() {}, offClick() {}, setParams() {}, showProgress() {}, hideProgress() {} })
        window.Telegram = { WebApp: { initData: 'query_id=x&hash=y', initDataUnsafe: {}, colorScheme: 'dark', themeParams: {}, BackButton: b(), MainButton: b(),
          ready() {}, expand() {}, close() {}, setHeaderColor() {}, setBackgroundColor() {}, onEvent() {}, offEvent() {}, downloadFile() {}, openLink() {} } } })()`,
    }),
  )
  await page.route('**/api/v1/**', async (route) => {
    const req = route.request()
    const p = new URL(req.url()).pathname.replace('/api/v1', '')
    requests.push(req.method() + ' ' + p + new URL(req.url()).search)
    if (p === '/telegram/session') return route.fulfill(json({ token: 'spaceman_tgs_test', expires_at: '2099-01-01T00:00:00Z', idle_timeout_seconds: 600 }, 201))
    if (p === '/telegram/me')
      return route.fulfill(json({ root: false, method: 'telegram', session_id: '', limited: false, needs_enroll: false, expires_at: '2099-01-01T00:00:00Z', domains: [SPACE],
        user: { id: 'usr_1', display_name: 'Test Person', enabled: true, grants: [{ domain: SPACE, role: o.role ?? 'operator' }] } }))
    if (p === '/watch') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': ok\n\n' })
    if (p === '/telegram/link/prefs' && req.method() === 'GET') return route.fulfill({ ...json(prefs), headers: { ETag: `"${prefs.version}"` } })
    if (p === '/telegram/link/prefs' && req.method() === 'PUT') {
      const ifMatch = (await req.allHeaders())['if-match']
      puts.push({ ifMatch, body: req.postDataJSON() })
      if (!ifMatch) return route.fulfill(json({ status: 428, code: 'precondition_required', title: 'x', detail: 'x' }, 428))
      if (conflicts-- > 0) {
        prefs = { ...prefs, version: (prefs.version as number) + 1, timezone: 'Europe/Paris' }
        return route.fulfill({ status: 412, contentType: 'application/problem+json', body: JSON.stringify({ status: 412, code: 'version_conflict', title: 'Precondition failed', detail: 'changed' }) })
      }
      const b = req.postDataJSON() as Record<string, unknown>
      prefs = { ...prefs, version: (prefs.version as number) + 1, timezone: (b.timezone as string) ?? prefs.timezone, overrides: { ...b, timezone: undefined }, effective: { ...(prefs.defaults as object), ...b, muted_spaces: b.muted_spaces ?? [] } }
      return route.fulfill(json(prefs))
    }
    if (p === `/spaces/${SPACE}/queue/next`) return route.fulfill(json({ selected: task(1), skipped: [{ task_id: T(2), title: 'Blocked one', reason: 'depends on Task number 1' }] }))
    if (p === '/spaces') return route.fulfill(json({ items: [{ id: SPACE, name: 'Pages space', state: 'active', version: 1 }] }))
    if (p === `/spaces/${SPACE}`) return route.fulfill(json({ id: SPACE, name: 'Pages space', state: 'active', version: 1, goals: 'Ship it', lease: { valid: true, state: 'valid', generation: 1 } }))
    if (p === '/attention') {
      const space = { space_id: SPACE, name: 'Pages space', slug: 'p', state: 'active', total: 0, ...o.attention }
      return route.fulfill(json({ total: space.total, checked_at: now, spaces: [space] }))
    }
    if (p === `/spaces/${SPACE}/runs`) return route.fulfill(json({ items: [run] }))
    if (p === `/spaces/${SPACE}/runs/${run.id}`) return route.fulfill(json(run))
    // The scheduled list is GET /messages?scheduled=true (api.GET in useScheduledMessages), not a path of its own.
    if (p === `/spaces/${SPACE}/messages`) {
      if (new URL(req.url()).searchParams.get('scheduled') === 'true') return route.fulfill(json({ items: o.scheduled ?? [] }))
      return route.fulfill(json({ items: [message], operators_pending: 1 }))
    }
    if (p === `/spaces/${SPACE}/tasks` && req.method() === 'POST') {
      created.push(req.postDataJSON())
      return route.fulfill(json(task(9), 201))
    }
    if (p === `/spaces/${SPACE}/tasks`) return route.fulfill(json({ tasks: o.tasks ?? [task(1), task(2), task(3, 'running')], policy: { max_active: 1 } }))
    if (p.endsWith('/move')) {
      moves.push({ path: p, body: req.postDataJSON() })
      return route.fulfill(json({ ...task(1) }))
    }
    return route.fulfill(json({ items: [], counts: { new: 0, in_progress: 0, done: 0, rejected: 0, deferred: 0 } }))
  })
  await page.goto(`http://127.0.0.1:${PORT}${o.path}`)
  return { moves, created, requests, puts }
}

async function clean(page: Page) {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow, 'no sideways page scroll').toBeLessThanOrEqual(0)
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(res.violations.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(' ')).join(' | ')}`)).toEqual([])
}

test('the start page lists spaces and links to settings', async ({ page }) => {
  await launch(page, { path: '/' })
  await expect(page.getByRole('link', { name: 'Pages space' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'implementor #2' })).toBeVisible()
  await clean(page)
})

test('the space overview has the Mini App sections only', async ({ page }) => {
  await launch(page, { path: `/spaces/${SPACE}` })
  const nav = page.getByRole('navigation', { name: 'Space sections' })
  await expect(nav.getByRole('link')).toHaveText([/Overview/, /Queue/, /Needs you/, /Inbox/, /Runs/, /Messages/])
  await expect(page.getByRole('heading', { name: 'Agents' })).toBeVisible()
  await clean(page)
})

test('the queue: touch drag reorders, an operator sees New task, and the sheet opens', async ({ page }) => {
  const seen = await launch(page, { path: `/spaces/${SPACE}/queue` })
  await expect(page.getByRole('listitem')).toHaveCount(3)
  await clean(page)
  const grip = page.getByRole('button', { name: /Drag Task number 1/ })
  const target = page.getByRole('listitem').nth(2)
  const box = (await target.boundingBox())!
  const at = { clientX: box.x + 40, clientY: box.y + box.height / 2 }
  await grip.dispatchEvent('pointerdown', { pointerType: 'touch', pointerId: 7, ...at })
  await grip.dispatchEvent('pointermove', { pointerType: 'touch', pointerId: 7, ...at })
  await grip.dispatchEvent('pointerup', { pointerType: 'touch', pointerId: 7, ...at })
  await expect.poll(() => seen.moves.length).toBe(1)
  expect(seen.moves[0]).toEqual({ path: `/spaces/${SPACE}/tasks/${T(1)}/move`, body: { after_id: T(3) } })
  await page.getByRole('button', { name: /^2\. Task number 2/ }).click()
  await expect(page.getByRole('dialog').getByText('Do the')).toBeVisible()
  // The operator's state change is there, so its absence for a viewer means something.
  await expect(page.getByRole('button', { name: 'Change state' })).toBeVisible()
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: 'New task' }).click()
  await expect(page.getByRole('dialog', { name: 'New task' })).toBeVisible()
  await page.getByRole('dialog').getByRole('textbox', { name: 'Title' }).fill('From the phone')
  await page.getByRole('button', { name: 'Add to queue' }).click()
  await expect.poll(() => seen.created.length).toBe(1)
})

test('a viewer sees the queue without New task or drag handles', async ({ page }) => {
  await launch(page, { role: 'viewer', path: `/spaces/${SPACE}/queue` })
  await expect(page.getByRole('listitem')).toHaveCount(3)
  await expect(page.getByRole('button', { name: 'New task' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: /Drag / })).toHaveCount(0)
  await clean(page)
  await page.getByRole('button', { name: /^2\. Task number 2/ }).click()
  await expect(page.getByRole('dialog').getByText('Do the')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Change state' })).toHaveCount(0)
  await page.keyboard.press('Escape')
})

const proposed = { ...task(4), incident_proposed_by: 'run_pages1' }

test('an operator confirms or dismisses an incident proposal from the sheet', async ({ page }) => {
  await launch(page, { path: `/spaces/${SPACE}/queue`, tasks: [proposed] })
  await page.getByRole('button', { name: /^4\. Task number 4/ }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Incident proposal' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Confirm incident' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Dismiss' })).toBeVisible()
})

test('a viewer sees an incident proposal without its confirm or dismiss', async ({ page }) => {
  await launch(page, { role: 'viewer', path: `/spaces/${SPACE}/queue`, tasks: [proposed] })
  await page.getByRole('button', { name: /^4\. Task number 4/ }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Incident proposal' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Confirm incident' })).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Dismiss' })).toHaveCount(0)
  await clean(page)
})

test('the space overview names the web-only counts without links in the Mini App', async ({ page }) => {
  await launch(page, { path: `/spaces/${SPACE}`, attention: { total: 4, quarantine_open: 2, budget_exceeded: 1, tasks_blocked: 1 } })
  await expect(page.getByText('2 quarantined events')).toBeVisible()
  await expect(page.getByText('1 budgets exceeded')).toBeVisible()
  await expect(page.getByRole('link', { name: /quarantined events/ })).toHaveCount(0)
  await expect(page.getByRole('link', { name: /budgets exceeded/ })).toHaveCount(0)
  // Exact name: the Mini App header's "‹ All spaces" back link to the start page is fine; the web's /attention link is not.
  await expect(page.getByRole('link', { name: 'All spaces', exact: true })).toHaveCount(0)
  await expect(page.getByRole('link', { name: '1 blocked tasks' })).toBeVisible()
  await clean(page)
})

test('runs: the list and a read-only run page', async ({ page }) => {
  await launch(page, { path: `/spaces/${SPACE}/runs` })
  await expect(page.getByRole('link', { name: 'implementor #2' })).toBeVisible()
  await clean(page)
  await page.getByRole('link', { name: 'implementor #2' }).click()
  await expect(page.getByRole('heading', { name: 'implementor #2' })).toBeVisible()
  await expect(page.getByText(/terminal is not available/)).toBeVisible()
  await clean(page)
})

test('messages', async ({ page }) => {
  await launch(page, { path: `/spaces/${SPACE}/messages` })
  await expect(page.getByText('Please look at the deploy')).toBeVisible()
  await clean(page)
})

test('a scheduled message opens without the web-only cancel button in the Mini App', async ({ page }) => {
  const later = { ...message, id: 'msg_sched1', subject: 'Remind me on Friday', state: 'stored', deliver_at: '2099-01-02T00:00:00Z', from_kind: 'control', to_kind: 'control' }
  await launch(page, { path: `/spaces/${SPACE}/messages`, scheduled: [later] })
  await page.getByText('Scheduled (1)').click()
  await page.getByRole('listitem', { name: /Remind me on Friday/ }).click()
  await expect(page.getByRole('dialog').getByText(/control is not told before then/)).toBeVisible()
  await expect(page.getByRole('button', { name: 'Cancel scheduled message' })).toHaveCount(0)
  await clean(page)
})

test('Telegram settings', async ({ page }) => {
  await launch(page, { path: '/settings' })
  await expect(page.getByText('Test Person')).toBeVisible()
  await clean(page)
})

test('the queue shows the Next card and never sends record=true', async ({ page }) => {
  const seen = await launch(page, { path: `/spaces/${SPACE}/queue` })
  const card = page.getByLabel('Next task')
  await expect(card).toContainText('Task number 1')
  await expect(card).toContainText('1 task waits')
  await clean(page)
  expect(seen.requests.some((r) => r.includes('/queue/next'))).toBe(true)
  expect(seen.requests.filter((r) => r.includes('record'))).toEqual([])
})

test('settings: edit and save round-trips with If-Match, and topics are read-only', async ({ page }) => {
  const seen = await launch(page, { path: '/settings' })
  const form = page.getByRole('form', { name: 'Notification settings' })
  await expect(form.getByText(/Per user, not per link/)).toBeVisible()
  await expect(form.getByText('Not available yet')).toBeVisible()
  await clean(page)
  await form.getByLabel('New inbox items').selectOption('push')
  await form.getByLabel('Pages space').check()
  await form.getByLabel('Time zone').fill('Europe/London')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByText('Your Telegram notification settings were saved.')).toBeVisible()
  expect(seen.puts).toHaveLength(1)
  expect(seen.puts[0].ifMatch).toBe('"3"')
  expect(seen.puts[0].body).toEqual({ inbox_items: 'push', muted_spaces: [SPACE], timezone: 'Europe/London' })
  await expect(form.getByLabel('New inbox items')).toHaveValue('push')
  await expect(form.getByLabel('Time zone')).toHaveValue('Europe/London')
  expect(seen.requests.filter((r) => r.includes('record'))).toEqual([])
})

test('settings: a stale save (412) reloads the form and shows a conflict notice', async ({ page }) => {
  const seen = await launch(page, { path: '/settings', putStatus: 412 })
  const form = page.getByRole('form', { name: 'Notification settings' })
  await form.getByLabel('New inbox items').selectOption('push')
  await form.getByRole('button', { name: 'Save' }).click()
  await expect(page.getByRole('alert').filter({ hasText: 'Changed elsewhere' })).toBeVisible()
  await expect(form.getByLabel('Time zone')).toHaveValue('Europe/Paris')
  await expect(form.getByLabel('New inbox items')).toHaveValue('digest')
  expect(seen.puts.map((p) => p.ifMatch)).toEqual(['"3"'])
  await clean(page)
  // Saving again uses the reloaded version.
  await form.getByRole('button', { name: 'Save' }).click()
  await expect.poll(() => seen.puts.length).toBe(2)
  expect(seen.puts[1].ifMatch).toBe('"4"')
  expect(seen.puts.every((p) => p.ifMatch)).toBe(true)
})
