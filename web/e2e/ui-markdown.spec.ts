import { expect, test } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { join } from 'node:path'

import { overflowProblems, realScrollbars } from './overflow'
import { open, seedItem } from './markdown'

/**
 * Rich Markdown in an inbox item, through the real UI and its CSP: a wide table, highlighted code with a copy button, a
 * Mermaid diagram, an attachment image, footnotes and heading anchors, in both themes, at phone (390 px) and desktop
 * widths. Nothing scrolls the page sideways (wide content scrolls inside its block), the diagram renders as SVG in the
 * app's theme, the highlighter and mermaid load as lazy chunks, and the page logs no errors or CSP violations.
 */

test.use({ launchOptions: realScrollbars })

const shots = process.env.SM_SHOTS ?? join(process.cwd(), 'test-results', 'shots')

function body(attachment: string): string {
  const wide = Array.from({ length: 12 }, (_, i) => `settlement_column_${i}`)
  return [
    '## Settlement retries',
    '',
    `Retries **exhaust** the budget ~~early~~ during the bank window[^1]. ![batch chart](spaceman://attachment/${attachment})`,
    '',
    `| ${wide.join(' | ')} |`,
    `| ${wide.map(() => '---').join(' | ')} |`,
    `| ${wide.map((_, i) => `value-${i}-${'x'.repeat(12)}`).join(' | ')} |`,
    '',
    '- [x] Reproduce',
    '- [ ] Fix',
    '',
    '```go',
    'func retryBudget(batch *Batch) int { return batch.Size * 5 } // a long line that has to scroll inside its own block on a phone screen',
    '```',
    '',
    '```mermaid',
    'flowchart LR',
    '  A[Bank window opens] --> B{Retry budget left?}',
    '  B -- yes --> C[Retry the batch]',
    '  B -- no --> D[Alert the operator]',
    '```',
    '',
    '[^1]: Between 01:00 and 06:00 UTC.',
  ].join('\n')
}

test('an inbox item renders tables, code, a diagram and an image in both themes without overflow', async ({ browser }) => {
  test.setTimeout(180_000)
  mkdirSync(shots, { recursive: true })
  // Seed through the API in one context, then reuse its cookies in a context per width and theme.
  const { base, state, sp, att, item } = await seedItem(browser, 'markdown', 'Settlement retries exhaust the budget', body)

  const failures: string[] = []
  for (const width of [390, 1280]) {
    for (const theme of ['dark', 'light'] as const) {
      const ctx = await browser.newContext({ baseURL: base, storageState: state, viewport: { width, height: width < 768 ? 844 : 900 }, colorScheme: theme })
      const page = await ctx.newPage()
      const errors: string[] = []
      page.on('pageerror', (e) => errors.push(`pageerror: ${e.message}`))
      page.on('console', (m) => {
        if (m.type() === 'error') errors.push(`console: ${m.text()}`)
      })
      const scripts: string[] = []
      page.on('request', (r) => {
        if (r.resourceType() === 'script') scripts.push(new URL(r.url()).pathname)
      })
      await open(page, `/spaces/${sp.id}/inbox/${item.id}`, theme)
      const md = page.locator('main .sm-markdown').first()

      // GFM: heading anchor, strikethrough, task list, footnote, table in its own scroll block.
      await expect(md.getByRole('heading', { name: /Settlement retries/ })).toHaveAttribute('id', /^user-content-.*settlement-retries$/)
      await expect(md.locator('del')).toHaveText('early')
      await expect(md.getByRole('checkbox', { name: 'Reproduce' })).toBeDisabled()
      await expect(md.locator('section.footnotes li')).toContainText('Between 01:00 and 06:00 UTC.')
      await expect(md.locator('.sm-table table')).toBeVisible()

      // The attachment image loads through the session cookie.
      const img = md.getByRole('img', { name: 'batch chart' })
      await expect(img).toHaveAttribute('src', `/api/v1/spaces/${sp.id}/attachments/${att.id}/content`)
      await expect.poll(() => img.evaluate((e: HTMLImageElement) => e.complete && e.naturalWidth)).toBe(4)

      // Highlighted code with a copy button.
      await expect(md.locator('code[data-lang="go"] .th-keyword').first()).toHaveText('func')
      await expect(md.getByRole('button', { name: 'Copy code' })).toBeVisible()
      // The copy button never covers code: the first line starts below it.
      const overlap = await md.locator('.sm-codeblock').first().evaluate((block) => {
        const btn = block.querySelector('.sm-copy')!.getBoundingClientRect()
        const range = document.createRange()
        range.selectNodeContents(block.querySelector('code')!)
        const first = range.getClientRects()[0]
        return first.top < btn.bottom && first.left < btn.right && first.right > btn.left
      })
      expect(overlap).toBe(false)
      const color = await md.locator('code[data-lang="go"] .th-keyword').first().evaluate((e) => getComputedStyle(e).color)
      expect(color).toBe(theme === 'dark' ? 'rgb(199, 146, 234)' : 'rgb(153, 76, 195)')

      // The diagram renders as SVG (strict mode, sanitized), with its source as the text alternative.
      const fig = page.locator('main figure.sm-mermaid-figure')
      await expect(fig).toHaveAttribute('data-state', 'rendered', { timeout: 30_000 })
      await expect(fig.getByRole('img', { name: /Diagram/ }).locator('svg')).toBeVisible()
      await expect(fig.locator('svg')).toContainText('Retry budget left?')
      await expect(fig.locator('details.sm-mermaid-source code')).toContainText('flowchart LR')

      // The highlighter and mermaid came as lazy chunks, each loaded once.
      expect(scripts.filter((s) => /\/assets\/highlight-[\w-]+\.js$/.test(s))).toHaveLength(1)
      expect(scripts.filter((s) => /\/assets\/mermaid-[\w-]+\.js$/.test(s)).length).toBeGreaterThanOrEqual(1)

      await page.waitForTimeout(300)
      await page.screenshot({ path: join(shots, `markdown-${width}-${theme}.png`), fullPage: true })
      for (const p of await overflowProblems(page)) failures.push(`${width} px ${theme}: ${p.what}: ${p.el} (${p.scroll})`)
      for (const e of errors) failures.push(`${width} px ${theme}: ${e}`)
      await ctx.close()
    }
  }
  expect(failures).toEqual([])
})

