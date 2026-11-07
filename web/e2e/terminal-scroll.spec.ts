import { expect, test, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * Scrolling back through a live run terminal without taking it over (watch mode sends no input, SM-UI-004): the wheel
 * on a desktop and a touch drag at phone width move the terminal's scrollback, the view stays put while output keeps
 * arriving, and "Jump to live" goes back. During a takeover of a full-screen program that tracks the mouse (Claude
 * Code's full-screen renderer, the stub's "/fullscreen"), the wheel sends mouse-wheel reports, never arrow keys:
 * ghostty-web's arrow keys made Claude Code answer "Scroll wheel is sending arrow keys" instead of scrolling.
 */
test('scrolling back through a live terminal', async ({ page, browser }) => {
  test.setTimeout(150_000)
  const base = env('SM_URL')
  const shots = process.env.SM_SHOTS ?? 'test-results/shots'
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal scroll journey password'
  await asRoot.call('POST', '/users', { display_name: 'Scroll Tester', email: 'scroll@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('scroll@example.com', password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'scroll-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'scrollback', name: 'Scrollback', node_id: nodeId }, { 'Idempotency-Key': 'terminal-scroll-space' })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: 'scroll-stub', kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  // The node runs at most 4 workers: the runs started here are stopped at the end, whatever happens.
  const started: string[] = []
  const startRun = async (role: string, prompt: string, marker: string) => {
    const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role, harness: 'claude-code', prompt })
    started.push(run.id)
    await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${run.id}`)).observed, { timeout: 30_000 }).toBe('running')
    await expect.poll(async () => {
      const rec = await api.call<{ records: { k: string; d?: string }[] }>('GET', `/spaces/${sp.id}/runs/${run.id}/recording`)
      return rec.records.some((r) => r.k === 'o' && Buffer.from(r.d ?? '', 'base64').toString('latin1').includes(marker))
    }, { timeout: 20_000 }).toBe(true)
    return run.id
  }
  try {
    // A long reply (400 lines on a 48-row screen) followed by a tick every second.
    const long = await startRun('scroll', '/lines 400', 'reply line 400 of 400')

    // Any request a takeover would make; watch mode makes none.
    const takeoverRequests = (p: Page) => {
      const seen: string[] = []
      p.on('request', (r) => {
        if (r.url().includes('/takeover')) seen.push(`${r.method()} ${r.url()}`)
      })
      return seen
    }
    const scrollback = async (p: Page) => Number((await p.getByTestId('terminal').locator('..').getAttribute('data-scrollback')) ?? '0')

    // Desktop: the wheel scrolls the box, then the scrollback.
    const sent = takeoverRequests(page)
    await page.goto(`/spaces/${sp.id}/runs/${long}`)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
    const jump = page.getByTestId('jump-to-live')
    const box = page.getByTestId('terminal').locator('..')
    const bb = (await box.boundingBox())!
    await page.mouse.move(bb.x + bb.width / 2, bb.y + Math.min(bb.height / 2, 200))
    for (let i = 0; i < 6; i++) await page.mouse.wheel(0, -800)
    await expect.poll(() => scrollback(page), { timeout: 5_000 }).toBeGreaterThan(20)
    await expect(jump).toBeVisible()
    await page.screenshot({ path: `${shots}/terminal-scroll-desktop.png` })
    // New ticks arrive below; the viewer stays on the same lines, further up from the bottom.
    const held = await scrollback(page)
    await expect.poll(() => scrollback(page), { timeout: 10_000 }).toBeGreaterThan(held)
    await jump.click()
    await expect(jump).toBeHidden()
    await expect.poll(() => scrollback(page)).toBe(0)
    expect(sent).toEqual([])

    // Phone width with touch: a drag down the terminal scrolls back.
    const phone = await browser.newContext({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true, baseURL: base, colorScheme: 'dark', storageState: await page.context().storageState() })
    try {
      const mobile = await phone.newPage()
      const mobileSent = takeoverRequests(mobile)
      await mobile.goto(`/spaces/${sp.id}/runs/${long}`)
      await expect(mobile.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
      const mbox = mobile.getByTestId('terminal').locator('..')
      await mbox.scrollIntoViewIfNeeded()
      const mb = (await mbox.boundingBox())!
      // A finger dragged down the terminal twice (Chromium's synthesized scroll gestures send no touchmove events).
      const cdp = await phone.newCDPSession(mobile)
      const x = Math.round(mb.x + mb.width / 2)
      for (let i = 0; i < 2; i++) {
        let y = Math.round(mb.y + 40)
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] })
        for (let k = 0; k < 15; k++) {
          y += 20
          await cdp.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x, y }] })
        }
        await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
      }
      await expect.poll(() => scrollback(mobile), { timeout: 5_000 }).toBeGreaterThan(0)
      await expect(mobile.getByTestId('jump-to-live')).toBeVisible()
      await mobile.screenshot({ path: `${shots}/terminal-scroll-phone.png` })
      await mobile.getByTestId('jump-to-live').click()
      await expect.poll(() => scrollback(mobile)).toBe(0)
      expect(mobileSent).toEqual([])
    } finally {
      await phone.close()
    }

    // A full-screen program that tracks the mouse: watching, the wheel sends nothing.
    const full = await startRun('fullscreen-scroll', '/fullscreen', '[?1049h')
    // The takeover's input bodies, recorded in the page (the API client sends Request objects, whose bodies the
    // browser's network log leaves out).
    await page.addInitScript(() => {
      const w = window as unknown as { takeoverInput: string[] }
      w.takeoverInput = []
      const orig = window.fetch.bind(window)
      window.fetch = async (input, init) => {
        const req = new Request(input, init)
        if (req.method === 'POST' && /\/takeover\/[^/]+\/input$/.test(req.url)) w.takeoverInput.push(((await req.clone().json()) as { data: string }).data)
        return orig(req)
      }
    })
    const inputs = () => page.evaluate(() => (window as unknown as { takeoverInput: string[] }).takeoverInput.join(''))
    await page.goto(`/spaces/${sp.id}/runs/${full}`)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
    const fb = (await box.boundingBox())!
    await page.mouse.move(fb.x + fb.width / 2, fb.y + Math.min(fb.height / 2, 200))
    for (let i = 0; i < 4; i++) await page.mouse.wheel(0, -800)
    await page.waitForTimeout(500)
    expect(sent).toEqual([])

    // Taken over, the wheel reaches the program as SGR mouse-wheel reports.
    await page.getByRole('button', { name: 'Take over', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Take over', exact: true }).click()
    await expect(page.getByText('You have taken over this terminal')).toBeVisible()
    const tb = (await box.boundingBox())! // the takeover banner moved the terminal
    await page.mouse.move(tb.x + tb.width / 2, tb.y + Math.min(tb.height / 2, 200))
    for (let i = 0; i < 6; i++) await page.mouse.wheel(0, -800)
    await expect.poll(inputs, { timeout: 5_000 }).toContain('\x1b[<64;')
    expect(await inputs()).not.toContain('\x1b[A')
    const held2 = await api.call<{ takeover?: { id: string } }>('GET', `/spaces/${sp.id}/runs/${full}`)
    if (held2.takeover) await api.call('POST', `/spaces/${sp.id}/runs/${full}/takeover/${held2.takeover.id}/end`, {})
  } finally {
    for (const id of started) await api.call('POST', `/spaces/${sp.id}/runs/${id}/stop`, {}).catch(() => {})
  }
})
