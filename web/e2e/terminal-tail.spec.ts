import { expect, test, type Browser, type Page } from '@playwright/test'

import { instrument, redrawPrompt, replied, tailSpace, type TailProbe } from './tail'

/**
 * A terminal shows the end of a long recording first (ADR-0081): a live terminal's catch-up is the last 1 MiB of output
 * after the size and modes the output before it left, not the whole recording; earlier output loads when the viewer
 * scrolls to the top of the scrollback and asks for it. A replay shows its last screen from the same tail while the rest
 * loads, and a seek renders at most 1 MiB before its position.
 *
 * The stub's "/redraw n" repaints a status box n times the way Claude Code's classic renderer does (about 715 bytes a
 * frame) with a static line every 50 frames: much output, little scrollback, like a real session.
 */
test.use({ trace: 'off' })

const probe = (page: Page) => page.evaluate(() => (window as unknown as TailProbe).__smStreamed)
const text = (page: Page) => page.evaluate(() => (window as unknown as TailProbe).__smTerminals?.texts()[0] ?? '')
const screen = (page: Page) => page.evaluate(() => (window as unknown as TailProbe).__smTerminals?.screens()[0] ?? '')

test('a long terminal shows its latest output first and loads earlier output on scroll', async ({ page }) => {
  test.setTimeout(300_000)
  const { api, spaceId } = await tailSpace(page, 'tail')
  const { prompt } = redrawPrompt(7_000) // about 5 MB of output
  const run = await api.call<{ id: string }>('POST', `/spaces/${spaceId}/runs`, { kind: 'worker', role: 'tail', harness: 'claude-code', prompt })
  const runPath = `/spaces/${spaceId}/runs/${run.id}`
  try {
    await replied(api, spaceId, run.id)
    await page.addInitScript(instrument, 'tick \\d+')
    await page.goto(runPath)
    await expect(page.getByText('Live · watch-only')).toBeVisible({ timeout: 30_000 })
    await expect.poll(() => screen(page), { timeout: 60_000 }).toMatch(/^tick \d+$/m)
    await expect(page.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })

    // Only the tail came: its end is drawn, its start is not.
    const streamed = await probe(page)
    const box = page.locator('[data-earlier]')
    const earlier = Number(await box.getAttribute('data-earlier'))
    console.log(`live tail: ${streamed.terminal} bytes of events, ${earlier} bytes of output before them`)
    expect(streamed.terminal, 'bytes streamed for the catch-up').toBeLessThan(2_500_000)
    expect(earlier, 'output before the tail').toBeGreaterThan(3_000_000)
    let all = await text(page)
    expect(all).toContain('static line 140 of 140')
    expect(all).toMatch(/frame +7000 of +7000/)
    expect(all).not.toMatch(/^static line 1 of 140 *$/m)

    // Scroll to the top of the scrollback: earlier output is offered, and loads (1, then 4, then 16 MiB: all of it).
    const area = page.getByTestId('terminal')
    for (let i = 0; i < 5 && Number(await box.getAttribute('data-earlier')) > 0; i++) {
      await area.hover()
      await page.mouse.wheel(0, -1_000_000)
      await page.mouse.wheel(0, -1_000_000)
      const more = page.getByTestId('load-earlier')
      await expect(more).toBeVisible({ timeout: 10_000 })
      const before = Number(await box.getAttribute('data-earlier'))
      const top = Number(await box.getAttribute('data-scrollback'))
      await more.click()
      await expect(box).toHaveAttribute('data-reloading', 'false', { timeout: 60_000 })
      expect(Number(await box.getAttribute('data-earlier'))).toBeLessThan(before)
      // The viewer stays on the lines they were reading, with more above them now.
      await expect.poll(async () => Number(await box.getAttribute('data-scrollback')), { timeout: 10_000 }).toBeGreaterThanOrEqual(top - 2)
    }
    expect(Number(await box.getAttribute('data-earlier')), 'all earlier output loaded').toBe(0)
    all = await text(page)
    expect(all).toMatch(/^static line 1 of 140 *$/m)
    expect(all).toContain('static line 140 of 140')
    await expect(page.getByText('Live · watch-only')).toBeVisible()

    // The replay of the finished run: its last screen from the tail, then seeking renders a window before the position.
    await api.call('POST', `${runPath}/stop`, {})
    await expect.poll(async () => (await api.call<{ observed: string }>('GET', runPath)).observed, { timeout: 30_000 }).not.toBe('running')
    await page.goto(`/terminals?space=${spaceId}&run=${run.id}`)
    await expect(page.getByTestId('replay-toolbar')).toBeVisible({ timeout: 30_000 })
    await expect.poll(() => screen(page), { timeout: 60_000 }).toMatch(/^tick \d+$/m)
    const seek = page.locator('.mantine-Slider-root')
    await expect(seek).toBeVisible({ timeout: 60_000 })
    await expect(page.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    const lastTick = Math.max(...[...(await screen(page)).matchAll(/^tick (\d+)$/gm)].map((m) => Number(m[1])))
    const b = (await seek.boundingBox())!
    await page.mouse.click(b.x + b.width / 2, b.y + b.height / 2)
    await expect(page.getByTestId('terminal-loading')).toBeHidden({ timeout: 60_000 })
    await expect
      .poll(async () => Math.max(...[...(await screen(page)).matchAll(/^tick (\d+)$/gm)].map((m) => Number(m[1]))), { timeout: 30_000 })
      .toBeLessThan(lastTick)
    all = await text(page)
    // Halfway through, the redraws have ended: the window before it holds their end, not their start.
    expect(all).toContain('static line 140 of 140')
    expect(all).not.toMatch(/^static line 1 of 140 *$/m)
  } finally {
    await api.call('POST', `${runPath}/stop`, {}).catch(() => {})
  }
})

/**
 * Before/after numbers (ADR-0081), only with SM_E2E_TAIL_MEASURE=1: bytes received and the time from navigation until
 * the end of the recording is on screen, for about 5 MB and 22 MB of output, live and replayed, on a desktop at 1× and
 * 4× CPU and on a 390 px phone screen at 4×. The same spec runs against an older server and UI (no tails).
 */
test('measure: time and bytes to the latest screen', async ({ page, browser }) => {
  test.skip(!process.env.SM_E2E_TAIL_MEASURE, 'set SM_E2E_TAIL_MEASURE=1 to measure')
  test.setTimeout(1_800_000)
  const { api, spaceId } = await tailSpace(page, 'tailmeasure')
  const runs: string[] = []
  const start = async (frames: number) => {
    const run = await api.call<{ id: string }>('POST', `/spaces/${spaceId}/runs`, { kind: 'worker', role: `m${frames}`, harness: 'claude-code', prompt: redrawPrompt(frames).prompt })
    runs.push(run.id)
    return run.id
  }
  try {
    const sizes = [
      { name: '5 MB', frames: 7_000 },
      { name: '22 MB', frames: 31_000 },
    ]
    const ids: Record<string, { live: string; done: string }> = {}
    for (const s of sizes) ids[s.name] = { live: await start(s.frames), done: await start(s.frames) }
    for (const s of sizes) {
      await replied(api, spaceId, ids[s.name].live)
      await replied(api, spaceId, ids[s.name].done)
      await api.call('POST', `/spaces/${spaceId}/runs/${ids[s.name].done}/stop`, {})
    }
    const state = await page.context().storageState()
    const rows: string[] = []
    for (const s of sizes) {
      for (const cfg of [
        { name: 'desktop 1×', rate: 1, width: 1400, height: 900 },
        { name: 'desktop 4×', rate: 4, width: 1400, height: 900 },
        { name: 'phone 390px 4×', rate: 4, width: 390, height: 844 },
      ]) {
        for (const mode of ['live', 'replay'] as const) {
          const path = mode === 'live' ? `/spaces/${spaceId}/runs/${ids[s.name].live}` : `/terminals?space=${spaceId}&run=${ids[s.name].done}`
          const r = await measure(browser, state, path, cfg)
          rows.push(`| ${s.name} | ${mode} | ${cfg.name} | ${(r.shown / 1000).toFixed(1)} s | ${(r.bytes / 1e6).toFixed(2)} MB | ${(r.total / 1e6).toFixed(2)} MB |`)
          console.log(rows.at(-1))
        }
      }
    }
    console.log(['| Recording | View | Device | Latest screen after | Received by then | Received in all (10 s later) |', '|---|---|---|---|---|---|', ...rows].join('\n'))
  } finally {
    for (const id of runs) await api.call('POST', `/spaces/${spaceId}/runs/${id}/stop`, {}).catch(() => {})
  }
})

async function measure(browser: Browser, storageState: Awaited<ReturnType<ReturnType<Page['context']>['storageState']>>, path: string, cfg: { rate: number; width: number; height: number }) {
  const ctx = await browser.newContext({ storageState, viewport: { width: cfg.width, height: cfg.height } })
  try {
    const p = await ctx.newPage()
    await p.addInitScript(instrument, 'tick \\d+')
    const cdp = await ctx.newCDPSession(p)
    await cdp.send('Emulation.setCPUThrottlingRate', { rate: cfg.rate })
    await p.goto(path)
    await expect.poll(() => p.evaluate(() => (window as unknown as TailProbe).__smShown ?? 0), { timeout: 600_000, intervals: [500] }).toBeGreaterThan(0)
    const at = await p.evaluate(() => {
      const w = window as unknown as TailProbe
      return { shown: w.__smShown!, ...w.__smStreamed }
    })
    await p.waitForTimeout(10_000)
    const later = await probe(p)
    return { shown: at.shown, bytes: at.terminal + at.recording, total: later.terminal + later.recording }
  } finally {
    await ctx.close()
  }
}
