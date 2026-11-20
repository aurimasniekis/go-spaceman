import { expect, test } from '@playwright/test'

import { open, seedItem } from './markdown'

/**
 * Mermaid renders on the main thread, so a hostile body's diagrams are bounded (ADR-0082): small ones render on their
 * own and must each stay under DIAGRAM_TASK_MS of main-thread work in Chromium, large ones wait for a "Render diagram"
 * click, and ones over the limits never render.
 */

// Playwright's trace snapshots show up as long tasks of the page.
test.use({ trace: 'off' })

// The documented budget is about 1 s per auto-rendered diagram. The slowest kind (a 50-node mind map) measured 650-860 ms
// in nine runs on a busy node (0.7 s idle), and a run under load was seen at 1118 ms, 1.1× the base. The bound adds a 20%
// margin for that load (a run of the unwarmed spec still reached 1307 ms: the mind map's first slice also evaluated its
// cytoscape chunk, so the kinds are warmed up first): a doubled render of the slowest kind (1.3-1.7 s, 2.0 s when throttled
// 2×) still fails it, which a 50% margin (1.5 s) would not.
const DIAGRAM_BASE_MS = 1_000
const DIAGRAM_TASK_MS = Math.round(DIAGRAM_BASE_MS * 1.2)
// The first use of a costly kind evaluates its lazy chunks (0.1-0.4 s of loading and layout, split over several tasks).
const WARM_TASK_MS = 200

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
/**
 * One tiny diagram of each kind above: rendering them first loads and evaluates the kind's lazy chunks (a mind map pulls in
 * cytoscape and its layout, about 0.5 MB), so a measured slice is about rendering and not about the first use of a chunk.
 */
const warm = [
  'mindmap\n  root\n    a',
  'architecture-beta\nservice a(server)[A]',
  'block-beta\ncolumns 2\na b',
  'kanban\n  col\n    t[task]',
  'flowchart TD\nA-->B',
  'classDiagram\nclass A',
]
// About 5,000 px of text between the warm-up and the measured diagrams.
const SPACER = Array.from({ length: 150 }, (_, i) => `Spacer paragraph ${i}.`).join('\n\n')
const clickOnly = lines('flowchart TD', 300, (i) => `N${i}`)
const tooMany = 'graph TD\n' + Array.from({ length: 4000 }, (_, i) => `N${i}`).join('\n').slice(0, 9_990)

test('auto-rendered diagrams stay within their main-thread budget each; large ones wait for a click', async ({ browser }) => {
  test.setTimeout(240_000)
  const { base, state, sp, item } = await seedItem(browser, 'diagrams', 'Diagram budget', () =>
    [...warm, SPACER, ...auto, clickOnly, tooMany].map((d, i) => (d === SPACER ? d : `### Diagram ${i}\n\n${fence(d)}`)).join('\n\n'))
  // Exactly at the automatic limit (statements counted as planDiagrams does).
  for (const d of auto) {
    expect(d.length).toBeLessThanOrEqual(2_500)
    expect(d.split(/[\n;&]/).filter((p) => p.trim()).length).toBeLessThanOrEqual(50)
    expect(d.split(/[\n;&]/).filter((p) => p.trim()).length).toBeGreaterThanOrEqual(49)
  }
  const ctx = await browser.newContext({ baseURL: base, storageState: state, viewport: { width: 1280, height: 900 } })
  const page = await ctx.newPage()
  await page.addInitScript(() => {
    // Long tasks with their end time (on performance.now()).
    const w = window as unknown as { __tasks: { end: number; ms: number }[] }
    w.__tasks = []
    new PerformanceObserver((list) => {
      for (const e of list.getEntries()) w.__tasks.push({ end: e.startTime + e.duration, ms: e.duration })
    }).observe({ type: 'longtask', buffered: true })
  })
  await open(page, `/spaces/${sp.id}/inbox/${item.id}`, 'light')
  const figs = page.locator('main figure.sm-mermaid-figure')
  await expect(figs).toHaveCount(warm.length + auto.length + 2)
  // The warm-up diagrams come first: they load mermaid and each costly kind's chunks (evaluating a module is not a
  // diagram's work; the mind map's cytoscape and the architecture layout cost 0.1-0.4 s each). A spacer keeps the measured
  // diagrams far from the viewport (they render when near it), so none starts before the warm-up is done; `since` is taken
  // before the first of them is scrolled to. Renders queue one behind the other and yield between them (renderMermaid),
  // so each long task is one diagram's work.
  const t0 = await page.evaluate(() => performance.now())
  for (let i = 0; i < warm.length; i++) {
    await figs.nth(i).scrollIntoViewIfNeeded()
    await expect(figs.nth(i)).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
  }
  await page.waitForTimeout(100)
  const since = await page.evaluate(() => performance.now())
  const last = warm.length + auto.length - 1
  for (let i = warm.length; i <= last; i++) {
    await figs.nth(i).scrollIntoViewIfNeeded()
    await expect(figs.nth(i)).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
  }
  await page.waitForTimeout(100)
  const [warmTasks, tasks] = await page.evaluate(([from, to]) => {
    const w = window as unknown as { __tasks: { end: number; ms: number }[] }
    const ms = (xs: { end: number; ms: number }[]) => xs.map((x) => Math.round(x.ms))
    return [ms(w.__tasks.filter((x) => x.end > from && x.end <= to)), ms(w.__tasks.filter((x) => x.end > to))]
  }, [t0, since])
  console.log('long tasks while warming up (ms):', JSON.stringify(warmTasks))
  // The first use of each kind (its chunks) is measured apart: the probe saw tasks of up to 151 ms, so the bound is
  // WARM_TASK_MS.
  expect(Math.max(0, ...warmTasks), 'longest main-thread task of a first use').toBeLessThan(WARM_TASK_MS)
  console.log('long tasks while rendering the auto-rendered diagrams (ms):', JSON.stringify(tasks))
  expect(Math.max(0, ...tasks), 'longest main-thread task of an auto-rendered diagram').toBeLessThan(DIAGRAM_TASK_MS)

  // A large diagram waits for a click; the 4,000-node one never renders.
  const big = figs.nth(warm.length + auto.length)
  await big.scrollIntoViewIfNeeded()
  await expect(big).toHaveAttribute('data-state', 'waiting')
  await expect(big.locator('svg')).toHaveCount(0)
  const huge = figs.nth(warm.length + auto.length + 1)
  await huge.scrollIntoViewIfNeeded()
  await expect(huge).toHaveAttribute('data-state', 'error')
  await expect(huge.getByRole('status')).toContainText('more than 500 statements')
  await big.getByRole('button', { name: 'Render diagram' }).click()
  await expect(big).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
  await expect(big.locator('svg')).toBeVisible()
  await ctx.close()
})
