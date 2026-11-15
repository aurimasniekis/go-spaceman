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

async function launch(page: Page, o: { role?: string; path: string }) {
  const moves: unknown[] = []
  const created: unknown[] = []
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
    if (p === '/telegram/session') return route.fulfill(json({ token: 'spaceman_tgs_test', expires_at: '2099-01-01T00:00:00Z', idle_timeout_seconds: 600 }, 201))
    if (p === '/telegram/me')
      return route.fulfill(json({ root: false, method: 'telegram', session_id: '', limited: false, needs_enroll: false, expires_at: '2099-01-01T00:00:00Z', domains: [SPACE],
        user: { id: 'usr_1', display_name: 'Test Person', enabled: true, grants: [{ domain: SPACE, role: o.role ?? 'operator' }] } }))
    if (p === '/watch') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': ok\n\n' })
    if (p === '/spaces') return route.fulfill(json({ items: [{ id: SPACE, name: 'Pages space', state: 'active', version: 1 }] }))
    if (p === `/spaces/${SPACE}`) return route.fulfill(json({ id: SPACE, name: 'Pages space', state: 'active', version: 1, goals: 'Ship it', lease: { valid: true, state: 'valid', generation: 1 } }))
    if (p === '/attention') return route.fulfill(json({ total: 0, checked_at: now, spaces: [{ space_id: SPACE, name: 'Pages space', slug: 'p', state: 'active', total: 0 }] }))
    if (p === `/spaces/${SPACE}/runs`) return route.fulfill(json({ items: [run] }))
    if (p === `/spaces/${SPACE}/runs/${run.id}`) return route.fulfill(json(run))
    if (p === `/spaces/${SPACE}/messages`) return route.fulfill(json({ items: [message], operators_pending: 1 }))
    if (p === `/spaces/${SPACE}/messages/scheduled`) return route.fulfill(json({ items: [] }))
    if (p === `/spaces/${SPACE}/tasks` && req.method() === 'POST') {
      created.push(req.postDataJSON())
      return route.fulfill(json(task(9), 201))
    }
    if (p === `/spaces/${SPACE}/tasks`) return route.fulfill(json({ tasks: [task(1), task(2), task(3, 'running')], policy: { max_active: 1 } }))
    if (p.endsWith('/move')) {
      moves.push({ path: p, body: req.postDataJSON() })
      return route.fulfill(json({ ...task(1) }))
    }
    return route.fulfill(json({ items: [], counts: { new: 0, in_progress: 0, done: 0, rejected: 0, deferred: 0 } }))
  })
  await page.goto(`http://127.0.0.1:${PORT}${o.path}`)
  return { moves, created }
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

test('Telegram settings', async ({ page }) => {
  await launch(page, { path: '/settings' })
  await expect(page.getByText('Test Person')).toBeVisible()
  await clean(page)
})
