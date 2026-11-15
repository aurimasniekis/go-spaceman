import { readdirSync, readFileSync, statSync } from 'node:fs'
import { createServer, type Server } from 'node:http'
import { extname, join, relative } from 'node:path'

import AxeBuilder from '@axe-core/playwright'
import { expect, test } from '@playwright/test'

/**
 * Accessibility of the documentation site (SM-DOCS-005): every built page is audited with axe against WCAG 2.1 A/AA
 * in both color schemes. Runs only with SM_SITE_DIST (the built site), through `make site-check`.
 *
 * The pages are split over SM_A11Y_SHARDS parallel tests (default 1; `make site-a11y` picks a count from the CPUs and
 * runs as many workers). Each test walks its pages in one tab of one browser context, first in the dark scheme, then
 * in the light one, with one init script. One context per worker on purpose: Chromium in --single-process mode
 * (SM_CHROMIUM_ARGS on sandboxed hosts) is gone once its first context closes, so a second one fails to open.
 */
const dist = process.env.SM_SITE_DIST ?? ''
const shards = Math.max(1, Math.floor(Number(process.env.SM_A11Y_SHARDS)) || 1)
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

/** Serves the built site under BASE on a free loopback port. */
async function serve(): Promise<{ server: Server; port: number }> {
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
  return { server, port: (server.address() as { port: number }).port }
}

/**
 * A page is ready for the audit once VitePress has hydrated (Vue sets __vue_app__ on #app when it mounts), the fonts
 * have loaded and every Mermaid diagram (rendered in the browser after mounting) has its SVG.
 */
function vitepressReady(): boolean {
  return (
    !!(document.getElementById('app') as (HTMLElement & { __vue_app__?: unknown }) | null)?.__vue_app__ &&
    document.fonts.status === 'loaded' &&
    [...document.querySelectorAll('.mermaid')].every((d) => d.querySelector('svg'))
  )
}

test.describe.configure({ mode: 'parallel' })

for (let shard = 0; shard < shards; shard++) {
  test(`documentation site accessibility (shard ${shard + 1}/${shards})`, async ({ context, page }) => {
    test.skip(!dist, 'set SM_SITE_DIST to the built site')
    // Both schemes of the whole site take about 4 minutes on GitHub's runners in one shard; allow 15 for slow hosts.
    test.setTimeout(900_000)
    // One init script for the whole context: VitePress's stored appearance follows the emulated color scheme.
    await context.addInitScript(() => {
      try {
        localStorage.setItem('vitepress-theme-appearance', matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
      } catch {
        // about:blank has no storage.
      }
    })
    const all = pages(dist).sort()
    expect(all.length, 'built pages').toBeGreaterThan(0)
    const mine = all.filter((_, i) => i % shards === shard)
    const { server, port } = await serve()
    const problems: string[] = []
    try {
      for (const scheme of ['dark', 'light'] as const) {
        await page.emulateMedia({ colorScheme: scheme })
        for (const file of mine) {
          const url = `http://127.0.0.1:${port}${BASE}${relative(dist, file).replace(/index\.html$/, '').replace(/\.html$/, '')}`
          await page.goto(url, { waitUntil: 'load' })
          await page.waitForFunction(vitepressReady, undefined, { timeout: 30_000 })
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
}
