import { expect, test, type Page } from '@playwright/test'
import { createReadStream, existsSync, statSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { extname, join, normalize } from 'node:path'

/**
 * The Telegram Mini App client in a real browser, without a Telegram bot: the built `dist/` (tg.html) is served by a small
 * local server, Telegram's loader is replaced by a stub WebApp, and the API is mocked at the network. It checks what the
 * unit tests cannot: images arrive as blob: URLs fetched with the Telegram header (and only once visible), an attachment
 * link mints a signed download instead of navigating, the theme maps onto the page, the Back/Main buttons and the start
 * parameter work, and a 429 on a download is a notice. Needs `make web` output in web/dist (`pnpm build`).
 * Run alone: `SPACEMAN_*` unset, `pnpm exec playwright test e2e/tg-miniapp.spec.ts`.
 */

const PORT = 39731
const dist = join(process.cwd(), 'dist')
const SPACE = 'spc_miniapp1'
const ITEM = 'inb_miniapp1'
const IMG = 'att_' + 'a'.repeat(26)
const FILE = 'att_' + 'b'.repeat(26)
const LATE = 'att_' + 'c'.repeat(26)
// A 1x1 PNG.
const PNG = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==', 'base64')

let server: Server
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
  await new Promise<void>((r) => server.listen(PORT, '127.0.0.1', r))
})
test.afterAll(() => new Promise<void>((r) => server.close(() => r())))

const problem = (status: number, code: string, headers: Record<string, string> = {}) => ({
  status,
  headers: { 'Content-Type': 'application/problem+json', ...headers },
  body: JSON.stringify({ status, code, title: code, detail: code }),
})
const json = (body: unknown, status = 200) => ({ status, contentType: 'application/json', body: JSON.stringify(body) })

interface Setup {
  startParam?: string
  downloadStatus?: number
  scheme?: 'light' | 'dark'
  path?: string
}

async function launch(page: Page, o: Setup = {}) {
  const seen = { authHeaders: [] as Array<string | undefined>, contentRequests: [] as string[], cookies: [] as Array<string | undefined>, downloads: 0 }
  await page.route('https://telegram.org/js/telegram-web-app.js', (r) =>
    r.fulfill({
      contentType: 'text/javascript',
      body: `(() => {
        const log = (window.__tg = { back: [], main: [], downloads: [], links: [] })
        const btn = (name) => { const hs = new Set(); return { isVisible: false, show() { log[name].push('show') }, hide() { log[name].push('hide') }, onClick(cb) { hs.add(cb) }, offClick(cb) { hs.delete(cb) }, setParams(p) { log[name].push(p) }, showProgress() {}, hideProgress() {}, fire() { hs.forEach((h) => h()) } } }
        const themeHandlers = new Set()
        window.Telegram = { WebApp: {
          initData: 'query_id=x&hash=y${o.startParam ? `&start_param=${o.startParam}` : ''}',
          initDataUnsafe: {}, colorScheme: ${JSON.stringify(o.scheme ?? 'dark')},
          themeParams: { bg_color: '#102030', text_color: '#f0e0d0', hint_color: '#a0b0c0', link_color: '#40a0ff', button_color: '#2481cc', button_text_color: '#ffffff', secondary_bg_color: '#182838' },
          BackButton: btn('back'), MainButton: btn('main'),
          ready() {}, expand() {}, close() {}, setHeaderColor() {}, setBackgroundColor() {},
          onEvent(e, cb) { themeHandlers.add(cb) }, offEvent(e, cb) { themeHandlers.delete(cb) },
          downloadFile(p, cb) { log.downloads.push(p); cb && cb(true) }, openLink(u) { log.links.push(u) },
        } }
      })()`,
    }),
  )
  await page.route('**/api/v1/**', async (route) => {
    const req = route.request()
    const u = new URL(req.url())
    const p = u.pathname.replace('/api/v1', '')
    const h = await req.allHeaders()
    seen.authHeaders.push(h.authorization)
    seen.cookies.push(h.cookie)
    if (p === '/telegram/session') return route.fulfill(json({ token: 'spaceman_tgs_test', expires_at: '2099-01-01T00:00:00Z', idle_timeout_seconds: 600 }, 201))
    if (h.authorization !== 'Telegram spaceman_tgs_test') return route.fulfill(problem(401, 'unauthorized'))
    if (p === '/telegram/me') return route.fulfill(json({ root: false, method: 'telegram', session_id: '', limited: false, needs_enroll: false, expires_at: '2099-01-01T00:00:00Z', domains: [SPACE], user: { id: 'usr_1', name: 'Test', enabled: true, grants: [{ domain: SPACE, role: 'operator' }] } }))
    if (p === '/watch') return route.fulfill({ status: 200, contentType: 'text/event-stream', body: ': ok\n\n' })
    if (p === `/spaces/${SPACE}/inbox/${ITEM}`)
      return route.fulfill(
        json({
          id: ITEM, space_id: SPACE, title: 'Mini App item', status: 'new', urgency: 'normal', source_kind: 'manual', received_at: '2026-11-14T10:00:00Z', version: 1, trusted: {}, attachments: [],
          body: `Look: ![chart](spaceman://attachment/${IMG})\n\n[the report](spaceman://attachment/${FILE})\n\n${'filler paragraph\n\n'.repeat(60)}![late](spaceman://attachment/${LATE})`,
        }),
      )
    if (p === `/spaces/${SPACE}/attachments/${IMG}/content` || p === `/spaces/${SPACE}/attachments/${LATE}/content`) {
      seen.contentRequests.push(p)
      return route.fulfill({ status: 200, contentType: 'image/png', body: PNG })
    }
    if (p === '/telegram/download-links') {
      seen.downloads++
      if (o.downloadStatus === 429) return route.fulfill(problem(429, 'rate_limited', { 'Retry-After': '3' }))
      return route.fulfill(json({ url: `http://127.0.0.1:${PORT}/tg-dl/secretid`, expires_at: '2099-01-01T00:00:00Z', max_uses: 3 }, 201))
    }
    if (p === `/spaces/${SPACE}`) return route.fulfill(json({ id: SPACE, name: 'Mini', version: 1 }))
    return route.fulfill(json({ items: [], counts: { new: 0, in_progress: 0, done: 0, rejected: 0, deferred: 0 } }))
  })
  await page.goto(`http://127.0.0.1:${PORT}${o.path ?? `/spaces/${SPACE}/inbox/${ITEM}`}`)
  return seen
}

test('images are blob URLs fetched with the Telegram header, and a far image waits until it is visible', async ({ page }) => {
  const seen = await launch(page)
  const img = page.locator('.sm-markdown img').first()
  await expect(img).toHaveAttribute('src', /^blob:/)
  await expect.poll(() => img.evaluate((el: HTMLImageElement) => el.naturalWidth)).toBe(1)
  expect(seen.contentRequests).toEqual([`/spaces/${SPACE}/attachments/${IMG}/content`])
  await page.locator('.sm-markdown img').last().scrollIntoViewIfNeeded()
  await expect.poll(() => seen.contentRequests.length).toBe(2)
  // Every API call after the exchange carried the Telegram scheme; none carried a cookie.
  expect(seen.authHeaders.slice(1).every((a) => a === 'Telegram spaceman_tgs_test')).toBe(true)
  expect(seen.cookies.every((c) => !c)).toBe(true)
})

test('an attachment link mints a signed download and hands it to Telegram; a 429 is a notice', async ({ page }) => {
  const seen = await launch(page)
  await page.getByRole('link', { name: 'the report' }).click()
  await expect.poll(() => page.evaluate(() => (window as unknown as { __tg: { downloads: unknown[] } }).__tg.downloads.length)).toBe(1)
  const dl = await page.evaluate(() => (window as unknown as { __tg: { downloads: Array<{ url: string }> } }).__tg.downloads[0])
  expect(dl.url).toBe(`http://127.0.0.1:${PORT}/tg-dl/secretid`)
  expect(seen.downloads).toBe(1)
  expect(page.url()).toContain(ITEM)
})

test('a 429 on a download shows a try-again notice and the page stays', async ({ page }) => {
  await launch(page, { downloadStatus: 429 })
  await page.getByRole('link', { name: 'the report' }).click()
  await expect(page.getByText(/Too many downloads at once. Try again in 3 s/)).toBeVisible()
  expect(await page.evaluate(() => (window as unknown as { __tg: { downloads: unknown[] } }).__tg.downloads.length)).toBe(0)
  await expect(page.getByRole('heading', { name: 'Mini App item' })).toBeVisible()
})

test('Telegram theme colours reach the page, and the Back button shows off the start page and goes back', async ({ page }) => {
  await launch(page, { scheme: 'light' })
  await expect(page.getByRole('heading', { name: 'Mini App item' })).toBeVisible()
  const body = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
  expect(body).toBe('rgb(16, 32, 48)')
  expect(await page.evaluate(() => document.documentElement.getAttribute('data-mantine-color-scheme'))).toBe('light')
  const log = () => page.evaluate(() => (window as unknown as { __tg: { back: unknown[] } }).__tg.back)
  await expect.poll(log).toContain('show')
  await page.evaluate(() => (history.pushState({}, '', '/'), void 0))
  await page.goBack()
  await page.evaluate(() => (window as unknown as { Telegram: { WebApp: { BackButton: { fire(): void } } } }).Telegram.WebApp.BackButton.fire())
  await expect(page).toHaveURL(`http://127.0.0.1:${PORT}/`)
})

test('the start parameter opens the page it names', async ({ page }) => {
  await launch(page, { startParam: `inbox-${SPACE}-${ITEM}`, path: '/' })
  await expect(page).toHaveURL(new RegExp(`/spaces/${SPACE}/inbox/${ITEM}$`))
  await expect(page.getByRole('heading', { name: 'Mini App item' })).toBeVisible()
})

test('a malformed start parameter is ignored', async ({ page }) => {
  await launch(page, { startParam: 'inbox-..%2F..-x', path: '/' })
  await expect(page.locator('#root')).not.toBeEmpty()
  expect(new URL(page.url()).pathname).toBe('/')
})
