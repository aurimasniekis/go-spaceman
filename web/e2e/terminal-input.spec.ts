import { devices, expect, test, type Page } from '@playwright/test'
import AxeBuilder from '@axe-core/playwright'
import { readFileSync } from 'node:fs'

import { Api, env, startAgent } from './stack'

/**
 * A terminal takes keyboard input only while the viewer holds a takeover (SM-UI-004). Watching (live, replay or
 * someone else's takeover), a tap must not focus the terminal's input (on a phone that opens the on-screen keyboard)
 * and keys must not be captured; taken over, the tap focuses it and keys reach the run.
 */
const phones = { 'Pixel 7': devices['Pixel 7'], 'iPhone 13': devices['iPhone 13'] }

/** The terminal's own input elements: the hidden textarea and the contenteditable terminal element. */
const inputState = (page: Page) =>
  page.evaluate(() => {
    const term = document.querySelector('[data-testid="terminal"]') as HTMLElement
    const ta = term.querySelector('textarea') as HTMLTextAreaElement
    const a = document.activeElement
    return {
      activeInsideTerminal: !!a && (a === term || term.contains(a)),
      activeIsInput: !!a && (a === ta || (a as HTMLElement).isContentEditable),
      editable: term.isContentEditable,
      taInputMode: ta.getAttribute('inputmode'),
      taReadOnly: ta.readOnly,
      taTabIndex: ta.getAttribute('tabindex'),
    }
  })

/** Fails on any axe WCAG 2.1 A/AA violation inside the terminal as shown (SM-UI-006), e.g. aria-allowed-attr. */
async function axeTerminal(page: Page, what: string) {
  const res = await new AxeBuilder({ page }).include('[data-testid="terminal"]').withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
  const found = res.violations.map((v) => `${v.id} [${v.impact}]: ${v.nodes.slice(0, 3).map((n) => n.target.join(' ')).join(' | ')}`)
  expect(found, `${what}: accessibility violations`).toEqual([])
}

test('the terminal takes input only during a takeover', async ({ page, browser }) => {
  test.setTimeout(150_000)
  const base = env('SM_URL')
  const asRoot = new Api(page.request, base, readFileSync(env('SM_ROOT_TOKEN_FILE'), 'utf8').trim())
  const password = 'terminal input journey password'
  await asRoot.call('POST', '/users', { display_name: 'Input Tester', email: 'input@example.com', password, grants: [{ role: 'admin', domain: '*' }] })
  const api = new Api(page.request, base)
  await api.signIn('input@example.com', password)

  const nodes = await api.call<{ items: { id: string; connected: boolean }[] }>('GET', '/nodes')
  let nodeId = nodes.items.find((n) => n.connected)?.id
  if (!nodeId) {
    const node = await api.call<{ node: { id: string }; token: string }>('POST', '/nodes', { name: 'input-node' })
    startAgent(node.token)
    nodeId = node.node.id
    await expect.poll(async () => (await api.call<{ connected: boolean }>('GET', `/nodes/${nodeId}`)).connected, { timeout: 30_000 }).toBe(true)
  }
  const sp = await api.call<{ id: string }>('POST', '/spaces', { slug: 'terminal-input', name: 'Terminal input', node_id: nodeId }, { 'Idempotency-Key': 'terminal-input-space' })
  const acct = await asRoot.call<{ id: string }>('POST', '/accounts', { name: 'input-stub', kind: 'claude_oauth_token', secret: 'stub-token' })
  await asRoot.call('PUT', `/spaces/${sp.id}/accounts`, { items: [{ account_id: acct.id, default: true }] })
  await expect.poll(async () => (await api.call<{ lease?: { valid: boolean } }>('GET', `/spaces/${sp.id}`)).lease?.valid, { timeout: 30_000 }).toBe(true)

  const started: string[] = []
  try {
    const run = await api.call<{ id: string }>('POST', `/spaces/${sp.id}/runs`, { kind: 'worker', role: 'input', harness: 'claude-code', prompt: '/lines 400' })
    started.push(run.id)
    await expect.poll(async () => (await api.call<{ observed: string }>('GET', `/spaces/${sp.id}/runs/${run.id}`)).observed, { timeout: 30_000 }).toBe('running')
    const url = `/spaces/${sp.id}/runs/${run.id}`

    // What the viewer's keys send to the run, recorded in the page.
    const record = (p: Page) =>
      p.addInitScript(() => {
        const w = window as unknown as { takeoverInput: string[] }
        w.takeoverInput = []
        const orig = window.fetch.bind(window)
        window.fetch = async (input, init) => {
          const req = new Request(input, init)
          if (req.method === 'POST' && /\/takeover\/[^/]+\/input$/.test(req.url)) w.takeoverInput.push(((await req.clone().json()) as { data: string }).data)
          return orig(req)
        }
      })
    const sent = (p: Page) => p.evaluate(() => (window as unknown as { takeoverInput: string[] }).takeoverInput.join(''))
    const takeOver = async (p: Page) => {
      await p.getByRole('button', { name: 'Take over', exact: true }).click()
      await p.getByRole('dialog').getByRole('button', { name: 'Take over', exact: true }).click()
      await expect(p.getByText('You have taken over this terminal')).toBeVisible()
    }
    const endTakeover = async () => {
      const held = await api.call<{ takeover?: { id: string } }>('GET', `/spaces/${sp.id}/runs/${run.id}`)
      if (held.takeover) await api.call('POST', `/spaces/${sp.id}/runs/${run.id}/takeover/${held.takeover.id}/end`, {})
    }

    // Phones: a tap on a watched terminal focuses nothing and shows no keyboard; after a takeover it focuses the input.
    for (const [name, device] of Object.entries(phones)) {
      const { defaultBrowserType: _ignored, ...options } = device
      const ctx = await browser.newContext({ ...options, baseURL: base, colorScheme: 'dark', storageState: await page.context().storageState() })
      try {
        const m = await ctx.newPage()
        await record(m)
        await m.goto(url)
        await expect(m.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
        const term = m.getByTestId('terminal')
        await term.scrollIntoViewIfNeeded()
        await term.tap()
        await m.waitForTimeout(300)
        const watched = await inputState(m)
        expect(watched, `${name} watching`).toMatchObject({ activeIsInput: false, editable: false, taInputMode: 'none', taReadOnly: true, taTabIndex: '-1' })
        await m.keyboard.type('hello')
        await m.keyboard.press('Enter')
        await m.waitForTimeout(300)
        expect(await sent(m), `${name} keys while watching`).toBe('')

        await takeOver(m)
        await m.getByTestId('terminal').scrollIntoViewIfNeeded()
        await m.getByTestId('terminal').tap()
        await expect.poll(() => inputState(m), { timeout: 5_000 }).toMatchObject({ activeInsideTerminal: true, editable: true, taReadOnly: false })
        expect((await inputState(m)).taInputMode).not.toBe('none')
        await m.keyboard.type('hi')
        await expect.poll(() => sent(m), { timeout: 5_000 }).toContain('hi')
        await endTakeover()
        // The takeover ended: the terminal is out of input mode again.
        await expect(m.getByText('You have taken over this terminal')).toBeHidden({ timeout: 20_000 })
        await expect.poll(() => inputState(m), { timeout: 5_000 }).toMatchObject({ editable: false, activeIsInput: false, taReadOnly: true })
      } finally {
        await ctx.close()
      }
    }

    // Desktop: clicking and typing on a watched terminal captures nothing; page keys keep their default action.
    await record(page)
    await page.goto(url)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 20_000 })
    await page.getByTestId('terminal').click()
    expect(await inputState(page)).toMatchObject({ activeIsInput: false, editable: false })
    const prevented = await page.evaluate(() => {
      const term = document.querySelector('[data-testid="terminal"]') as HTMLElement
      const out: boolean[] = []
      for (const key of ['a', 'Enter', 'ArrowUp', 'Tab', ' ']) {
        const e = new KeyboardEvent('keydown', { key, code: key === ' ' ? 'Space' : key.length === 1 ? `Key${key.toUpperCase()}` : key, bubbles: true, cancelable: true })
        term.dispatchEvent(e)
        out.push(e.defaultPrevented)
      }
      return out
    })
    expect(prevented).toEqual([false, false, false, false, false])
    await page.keyboard.type('hello')
    await page.keyboard.press('Enter')
    await page.waitForTimeout(300)
    expect(await sent(page)).toBe('')
    await axeTerminal(page, 'watched terminal')

    // Taken over, the same click focuses the input and typing reaches the run.
    await takeOver(page)
    await page.getByTestId('terminal').click()
    await expect.poll(() => inputState(page), { timeout: 5_000 }).toMatchObject({ activeInsideTerminal: true, editable: true })
    await page.keyboard.type('hi')
    await expect.poll(() => sent(page), { timeout: 5_000 }).toContain('hi')
    await axeTerminal(page, 'taken-over terminal')
    await endTakeover()
  } finally {
    for (const id of started) await api.call('POST', `/spaces/${sp.id}/runs/${id}/stop`, {}).catch(() => {})
  }
})
