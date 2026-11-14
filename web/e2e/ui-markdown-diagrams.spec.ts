import { expect, test } from '@playwright/test'

import { open, seedItem } from './markdown'

/**
 * Mermaid renders on the main thread, so a hostile body's diagrams are bounded (ADR-0082): small ones render on their
 * own and must each stay under about 1 s of main-thread work in Chromium, large ones wait for a "Render diagram" click,
 * and ones over the limits never render.
 */

// Playwright's trace snapshots show up as long tasks of the page.
test.use({ trace: 'off' })

const lines = (head: string, n: number, line: (i: number) => string) => [head, ...Array.from({ length: n }, (_, i) => line(i))].join('\n')
const fence = (src: string) => '```mermaid\n' + src + '\n```'

/**
 * The slowest diagrams that still render on their own (50 statements, 2,500 characters), one of each costly kind, then
 * a large one that waits for a click and the review's 4,000 bare node IDs that never render. Measured in Chromium.
 */
const auto = [
  lines('mindmap\n  root', 48, (i) => `    c${i}`),
  lines('architecture-beta', 49, (i) => `service s${i}(server)[S${i}]`),
  lines('block-beta\ncolumns 10', 48, (i) => `b${i}`),
  lines('kanban\n  col', 48, (i) => `    t${i}[task ${i}]`),
  lines('flowchart TD', 49, (i) => `N${i}`),
  'flowchart TD\n' + Array.from({ length: 48 }, (_, i) => `N${i}`).join(' & ') + ' --> Z',
  lines('classDiagram', 49, (i) => `class C${i}`),
  lines('flowchart TD', 49, (i) => `N${i}["${'label '.repeat(6)}${i}"]`),
]
const clickOnly = lines('flowchart TD', 300, (i) => `N${i}`)
const tooMany = 'graph TD\n' + Array.from({ length: 4000 }, (_, i) => `N${i}`).join('\n').slice(0, 9_990)

test('auto-rendered diagrams stay under 1 s of main-thread work each; large ones wait for a click', async ({ browser }) => {
  test.setTimeout(240_000)
  const { base, state, sp, item } = await seedItem(browser, 'diagrams', 'Diagram budget', () =>
    ['graph TD; A-->B', ...auto, clickOnly, tooMany].map((d, i) => `### Diagram ${i}\n\n${fence(d)}`).join('\n\n'))
  // Exactly at the automatic limit (statements counted as planDiagrams does).
  for (const d of auto) {
    expect(d.length).toBeLessThanOrEqual(2_500)
    expect(d.split(/[\n;&]/).filter((p) => p.trim()).length).toBeLessThanOrEqual(50)
    expect(d.split(/[\n;&]/).filter((p) => p.trim()).length).toBeGreaterThanOrEqual(49)
  }
  const ctx = await browser.newContext({ baseURL: base, storageState: state, viewport: { width: 1280, height: 900 } })
  const page = await ctx.newPage()
  await page.addInitScript(() => {
    const w = window as unknown as { __longTasks: number[] }
    w.__longTasks = []
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__longTasks.push(e.duration)
    }).observe({ type: 'longtask', buffered: true })
  })
  await open(page, `/spaces/${sp.id}/inbox/${item.id}`, 'light')
  const figs = page.locator('main figure.sm-mermaid-figure')
  await expect(figs).toHaveCount(auto.length + 3)
  // A small diagram first loads mermaid: evaluating the module is not a diagram's work.
  await expect(figs.first()).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
  // Diagrams near the viewport render ahead of the loop; each slice holds the tasks since the previous one, so together
  // they cover every long task after the warm-up.
  const tasks: { diagram: number; most: number }[] = []
  for (let i = 0; i < auto.length; i++) {
    const before = await page.evaluate(() => (window as unknown as { __longTasks: number[] }).__longTasks.length)
    await figs.nth(i + 1).scrollIntoViewIfNeeded()
    await expect(figs.nth(i + 1)).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
    await page.waitForTimeout(100)
    const most = await page.evaluate((n) => Math.max(0, ...(window as unknown as { __longTasks: number[] }).__longTasks.slice(n)), before)
    tasks.push({ diagram: i, most: Math.round(most) })
  }
  console.log('longest task per auto-rendered diagram (ms):', JSON.stringify(tasks))
  for (const t of tasks) expect(t.most, `diagram ${t.diagram}`).toBeLessThan(1_000)

  // A large diagram waits for a click; the 4,000-node one never renders.
  const big = figs.nth(auto.length + 1)
  await big.scrollIntoViewIfNeeded()
  await expect(big).toHaveAttribute('data-state', 'waiting')
  await expect(big.locator('svg')).toHaveCount(0)
  const huge = figs.nth(auto.length + 2)
  await huge.scrollIntoViewIfNeeded()
  await expect(huge).toHaveAttribute('data-state', 'error')
  await expect(huge.getByRole('status')).toContainText('more than 500 statements')
  await big.getByRole('button', { name: 'Render diagram' }).click()
  await expect(big).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
  await expect(big.locator('svg')).toBeVisible()
  await ctx.close()
})
