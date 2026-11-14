// Fails the build when the production bundle contains development-only code or inline scripts (the CSP forbids them),
// lacks the terminal's WebAssembly asset, or still inlines a WebAssembly module as a data: URL.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

const dist = new URL('../dist', import.meta.url).pathname
const bad = []
const walk = (d) => {
  for (const f of readdirSync(d)) {
    const p = join(d, f)
    if (statSync(p).isDirectory()) walk(p)
    else if (/\.(js|mjs|css|html)$/.test(f)) {
      const s = readFileSync(p, 'utf8')
      if (/ReactQueryDevtools|TanStackRouterDevtools/.test(s)) bad.push(`${p}: devtools in production bundle`)
      if (f.endsWith('.html') && /<script(?![^>]*\bsrc=)[^>]*>/.test(s)) bad.push(`${p}: inline <script>`)
      // ghostty-web's own copy of the module (vite.config.ts stripGhosttyInlineWasm): the terminal loads the asset.
      if (s.includes('data:application/wasm')) bad.push(`${p}: inlined data:application/wasm module`)
    }
  }
}
walk(dist)
// The Mini App shell (ADR-0083 §4.6): its only cross-origin script is Telegram's loader, by the exact URL the Mini App
// CSP pins (web/handler.go MiniAppScriptURL); every other script is same-origin ('self'). The web UI's page never loads it.
const goSrc = readFileSync(new URL('../handler.go', import.meta.url), 'utf8')
const pinned = /const MiniAppScriptURL = "([^"]+)"/.exec(goSrc)?.[1]
if (!pinned) bad.push('web/handler.go: MiniAppScriptURL not found')
const scriptSrcs = (html) => [...html.matchAll(/<script\b[^>]*\bsrc=["']([^"']+)["']/g)].map((m) => m[1])
// Same-origin means a path on this host. A protocol-relative `//host/x.js` starts with '/' but loads from another host.
const sameOrigin = (u) => u.startsWith('/') && !u.startsWith('//')
try {
  const tg = scriptSrcs(readFileSync(join(dist, 'tg.html'), 'utf8'))
  const foreign = tg.filter((u) => !sameOrigin(u))
  if (foreign.length !== 1 || foreign[0] !== pinned) bad.push(`dist/tg.html: cross-origin scripts ${JSON.stringify(foreign)}, want exactly [${pinned}]`)
  if (!tg.some((u) => u.startsWith('/assets/'))) bad.push('dist/tg.html: no hashed entry script')
  if (/<link\b[^>]*\bhref=["']https?:/.test(readFileSync(join(dist, 'tg.html'), 'utf8'))) bad.push('dist/tg.html: cross-origin <link> (CSP default-src)')
} catch {
  bad.push('dist/tg.html missing: the Mini App entry was not built')
}
if (scriptSrcs(readFileSync(join(dist, 'index.html'), 'utf8')).some((u) => !sameOrigin(u))) bad.push('dist/index.html: the web UI must not load a cross-origin script')
// The terminal's WebAssembly module is a hashed asset, fetched under the CSP's connect-src 'self' (ADR-0076).
if (!readdirSync(join(dist, 'assets')).some((f) => /^ghostty-vt-.*\.wasm$/.test(f))) bad.push(`${dist}/assets: no ghostty-vt-*.wasm`)
if (bad.length) {
  console.error(bad.join('\n'))
  process.exit(1)
}
console.log('dist ok')
