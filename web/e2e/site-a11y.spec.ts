import { readdirSync, readFileSync, statSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { extname, join, relative } from 'node:path'

import AxeBuilder from '@axe-core/playwright'
import { expect, test } from '@playwright/test'

/**
 * Accessibility of the documentation site (SM-DOCS-005): every built page is audited with axe against WCAG 2.1 A/AA
 * in both color schemes. Runs only with SM_SITE_DIST (the built site), through `make site-check`.
 */
const dist = process.env.SM_SITE_DIST ?? ''
const BASE = '/go-spaceman/'
const types: Record<string, string> = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png',
  '.woff2': 'font/woff2', '.json': 'application/json' }

function pages(dir: string): string[] {
  return readdirSync(dir).flatMap((f) => {
    const p = join(dir, f)
    if (statSync(p).isDirectory()) return f === 'assets' ? [] : pages(p)
    return f.endsWith('.html') && f !== '404.html' ? [p] : []
  })
}

test('documentation site accessibility', async ({ page }) => {
  test.skip(!dist, 'set SM_SITE_DIST to the built site')
  test.setTimeout(300_000)
  const server: Server = createServer((req, res) => {
    let path = decodeURIComponent((req.url ?? '/').split('?')[0])
    if (!path.startsWith(BASE)) {
      res.writeHead(404).end()
      return
    }
    path = path.slice(BASE.length)
    let file = join(dist, path)
    try {
      if (statSync(file).isDirectory()) file = join(file, 'index.html')
    } catch {
      file += '.html'
    }
    try {
      res.writeHead(200, { 'Content-Type': types[extname(file)] ?? 'application/octet-stream' }).end(readFileSync(file))
    } catch {
      res.writeHead(404).end()
    }
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', () => r()))
  const port = (server.address() as { port: number }).port
  const problems: string[] = []
  try {
    for (const file of pages(dist)) {
      const url = `http://127.0.0.1:${port}${BASE}${relative(dist, file).replace(/index\.html$/, '').replace(/\.html$/, '')}`
      for (const scheme of ['dark', 'light'] as const) {
        await page.emulateMedia({ colorScheme: scheme })
        await page.addInitScript((s) => localStorage.setItem('vitepress-theme-appearance', s), scheme)
        await page.goto(url)
        await page.waitForLoadState('networkidle')
        const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze()
        for (const v of res.violations) {
          const nodes = v.nodes.slice(0, 3).map((n) => `${n.target.join(' ')}${v.id === 'color-contrast' ? ` {${n.any[0]?.message ?? ''}}` : ''}`)
          problems.push(`${relative(dist, file)} (${scheme}): ${v.id} [${v.impact}] ${v.help}: ${nodes.join(' | ')}`)
        }
      }
    }
  } finally {
    server.close()
  }
  expect(problems, `accessibility violations:\n${problems.join('\n')}`).toEqual([])
})
