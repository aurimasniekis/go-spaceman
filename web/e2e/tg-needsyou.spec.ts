import AxeBuilder from '@axe-core/playwright'
import { expect, test, type Page } from '@playwright/test'
import { createReadStream, existsSync, statSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { extname, join, normalize } from 'node:path'

/**
 * The Mini App's needs-you forms and inbox at a phone's width (390 px) with axe, in a real browser and without a bot: the
 * built `dist/` (tg.html) is served locally, Telegram's loader is a stub and the API is mocked at the network (as
 * tg-miniapp.spec.ts does). Covers every human-action type (question, approval, to-do, message, a custom type with a
 * nested schema), the inbox list and item, and the "View diagram" page through MermaidDiagram. Needs `pnpm build` output.
 * Run alone: `pnpm exec playwright test e2e/tg-needsyou.spec.ts`.
 */

let PORT = 0 // an ephemeral port: set once the server listens
const dist = join(process.cwd(), 'dist')
const SPACE = 'spc_needs1'
const ITEM = 'inb_needs1'
let server: Server

test.use({ viewport: { width: 390, height: 780 } })

test.beforeAll(async () => {
  expect(existsSync(join(dist, 'tg.html')), 'run `pnpm build` first').toBe(true)
  server = createServer((req, res) => {
    const path = normalize(new URL(req.url ?? '/', 'http://x').pathname)
    let file = join(dist, path)
    if (!file.startsWith(dist) || !existsSync(file) || statSync(file).isDirectory()) file = join(dist, 'tg.html')
    const types: Record<string, string> = { '.js': 'text/javascript', '.css': 'text/css', '.html': 'text/html', '.svg': 'image/svg+xml', '.wasm': 'application/wasm', '.woff2': 'font/woff2' }
    res.setHeader('Content-Type', types[extname(file)] ?? 'application/octet-stream')
    createReadStream(file).pipe(res)
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  PORT = (server.address() as { port: number }).port
})
test.afterAll(() => new Promise<void>((r) => server.close(() => r())))

const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })
const problem = (status: number, code: string) => ({ status, headers: { 'Content-Type': 'application/problem+json' }, body: JSON.stringify({ status, code, title: code, detail: code }) })

const base = { space_id: SPACE, status: 'open', priority: 'normal', rank: 1, blocks_task: false, version: 1, reminder_count: 0, requester_kind: 'agent', requester_label: 'control', created_at: '2026-11-14T10:00:00Z', body: 'Please look at this.', attachments: [], comments: [], notifications: [] }
const SCHEMA = {
  type: 'object',
  required: ['site'],
  properties: {
    site: { type: 'object', title: 'Site', required: ['name'], properties: { name: { type: 'string', title: 'Site name' }, level: { $ref: '#/$defs/level' } } },
    contacts: { type: 'array', title: 'Contacts', items: { type: 'object', title: 'Contact', properties: { email: { type: 'string', title: 'Email' }, retries: { type: 'integer', title: 'Retries' } } } },
    mode: { title: 'Mode', oneOf: [{ const: 'a', title: 'Alpha' }, { const: 'b', title: 'Beta' }] },
    scores: { type: 'array', title: 'Scores', items: { type: 'number' } },
  },
  $defs: { level: { type: 'string', title: 'Level', enum: ['low', 'mid', 'high', 'max'] } },
}
const ACTIONS = [
  { ...base, id: 'hum_q1', type: 'question', title: 'Which region?', request: { options: ['eu', 'us'], allow_text: true } },
  { ...base, id: 'hum_a1', type: 'approval', title: 'Deploy to production', request: { action: 'deploy', details: 'Rolls out **v2**.' } },
  { ...base, id: 'hum_t1', type: 'todo', title: 'Rotate the key', request: { steps: ['Create', 'Swap'] } },
  { ...base, id: 'hum_m1', type: 'message', title: 'Heads up', request: { requires_ack: true } },
  { ...base, id: 'hum_c1', type: 'custom', type_slug: 'site-review', type_version: 1, title: 'Review the site', request: { site: { name: 'North' } },
    type_spec: { outcomes: ['approved', 'changes_requested'], request_schema: SCHEMA, response_schema: SCHEMA, ui: {} } },
]
const MERMAID = '```mermaid\ngraph TD\n  A[Start] --> B[Done]\n```'

async function launch(page: Page, path: string) {
  await page.route('https://telegram.org/js/telegram-web-app.js', (r) =>
    r.fulfill({
      contentType: 'text/javascript',
      body: `(() => {
        const btn = () => ({ isVisible: false, show() {}, hide() {}, onClick() {}, offClick() {}, setParams() {}, showProgress() {}, hideProgress() {} })
        window.Telegram = { WebApp: { initData: 'query_id=x&hash=y', initDataUnsafe: {}, colorScheme: 'dark',
          themeParams: { bg_color: '#102030', text_color: '#f0e0d0', hint_color: '#b0c0d0', link_color: '#7fc0ff', button_color: '#1a6bb3', button_text_color: '#ffffff', secondary_bg_color: '#182838' },
          BackButton: btn(), MainButton: btn(), ready() {}, expand() {}, close() {}, setHeaderColor() {}, setBackgroundColor() {}, onEvent() {}, offEvent() {}, downloadFile() {}, openLink() {} } }
      })()`,
    }),
  )
  await page.route('**/api/v1/**', async (route) => {
    const p = new URL(route.request().url()).pathname.replace('/api/v1', '')
    if (p === '/telegram/session') return route.fulfill(json({ token: 'spaceman_tgs_test', expires_at: '2099-01-01T00:00:00Z', idle_timeout_seconds: 600 }, 201))
    if (p === '/telegram/me') return route.fulfill(json({ root: false, method: 'telegram', session_id: '', limited: false, needs_enroll: false, expires_at: '2099-01-01T00:00:00Z', domains: [SPACE], user: { id: 'usr_1', name: 'Test', enabled: true, grants: [{ domain: SPACE, role: 'operator' }] } }))
    if (p === '/watch') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': ok\n\n' })
    const one = /\/human-actions\/(hum_\w+)$/.exec(p)
    if (one) return route.fulfill(json(ACTIONS.find((a) => a.id === one[1])))
    if (p.endsWith('/approval')) return route.fulfill(problem(404, 'not_found'))
    if (p === '/human-actions' || p === `/spaces/${SPACE}/human-actions`) return route.fulfill(json({ items: ACTIONS, counts: { [SPACE]: ACTIONS.length }, total: ACTIONS.length }))
    if (p === '/spaces') return route.fulfill(json({ items: [{ id: SPACE, name: 'Mini', slug: 'mini', state: 'active', node_id: 'nod_1', version: 1 }] }))
    if (p === `/spaces/${SPACE}/inbox/${ITEM}`)
      return route.fulfill(json({ id: ITEM, space_id: SPACE, title: 'Plan with a diagram', status: 'new', urgency: 'normal', source_kind: 'manual', content_type: 'text/markdown', received_at: '2026-11-14T10:00:00Z', version: 1, trusted: {}, attachments: [], body: `Here is the flow.\n\n${MERMAID}` }))
    if (p === `/spaces/${SPACE}/inbox`) return route.fulfill(json({ items: [{ id: ITEM, space_id: SPACE, title: 'Plan with a diagram that has a rather long title to wrap on a phone', status: 'new', urgency: 'high', source_kind: 'manual', content_type: 'text/markdown', received_at: '2026-11-14T10:00:00Z', version: 1, body: 'x', attachments: [] }], counts: { new: 1, in_progress: 0, done: 0, rejected: 0, deferred: 0 } }))
    return route.fulfill(json({ items: [], counts: {} }))
  })
  await page.goto(`http://127.0.0.1:${PORT}${path}`)
}

async function clean(page: Page) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  expect(res.violations.map((v) => `${v.id}: ${v.nodes.slice(0, 2).map((n) => n.target.join(' ') + ' ' + (n.any[0]?.message ?? '')).join(' | ')}`)).toEqual([])
  // No sideways scrolling of the page at 390 px.
  expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390)
}

test('needs-you lists every type and opens each form full width, without axe violations', async ({ page }) => {
  await launch(page, '/human')
  await expect(page.getByRole('heading', { name: 'Needs you' })).toBeVisible()
  await clean(page)
  const forms: Array<[string, string]> = [['hum_q1', 'Send answer'], ['hum_a1', 'Approve'], ['hum_t1', 'Mark completed'], ['hum_m1', 'Acknowledge'], ['hum_c1', 'approved']]
  for (const [id, button] of forms) {
    await page.goto(`http://127.0.0.1:${PORT}/spaces/${SPACE}/human?action=${id}`)
    await expect(page.getByRole('button', { name: button, exact: true })).toBeVisible()
    await clean(page)
  }
})

test('a custom type with a nested schema shows nested fields, repeated groups and options', async ({ page }) => {
  await launch(page, `/spaces/${SPACE}/human?action=hum_c1`)
  await expect(page.getByRole('group', { name: 'Site' }).first()).toBeVisible()
  await expect(page.getByLabel('Site name')).toBeVisible()
  await page.getByRole('button', { name: 'Add Contact' }).click()
  await expect(page.getByLabel('Email')).toBeVisible()
  await expect(page.getByRole('radio', { name: 'Alpha' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Remove Contact 1' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Policies' })).toHaveCount(0)
  await clean(page)
})

test('the inbox is a card list and an item with a diagram offers View diagram', async ({ page }) => {
  await launch(page, `/spaces/${SPACE}/inbox`)
  await expect(page.getByRole('list', { name: 'Inbox items' })).toBeVisible()
  await clean(page)
  await page.getByRole('link', { name: /Plan with a diagram/ }).click()
  await clean(page)
  await page.getByRole('button', { name: 'View diagram' }).click()
  await expect(page).toHaveURL(new RegExp(`/inbox/${ITEM}/diagram$`))
  await expect(page.locator('figure.sm-mermaid-figure')).toBeVisible()
  await expect(page.locator('figure.sm-mermaid-figure')).toHaveAttribute('data-state', 'rendered', { timeout: 20_000 })
  await clean(page)
})
