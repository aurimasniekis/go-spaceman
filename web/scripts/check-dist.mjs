// Fails the build when the production bundle contains development-only code or inline scripts (the CSP forbids them),
// lacks the terminal's WebAssembly asset, still inlines a WebAssembly module as a data: URL, or loads mermaid or the
// code highlighter eagerly (they are lazy chunks of the Markdown renderer). Prints the eager and lazy sizes.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { gzipSync } from 'node:zlib'

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

// Eager code: what index.html loads, and what the shared Markdown renderer's chunk (MarkdownView) imports statically.
// Mermaid and the highlighter must stay out of both: they load only for bodies that have a diagram or a code block.
const assets = join(dist, 'assets')
const read = (f) => readFileSync(join(assets, f), 'utf8')
const staticImports = (f) => [...read(f).matchAll(/(?:from|import)\s*["']\.\/([\w.-]+\.js)["']/g)].map((m) => m[1])
const closure = (roots) => {
  const seen = new Set()
  const todo = [...roots]
  while (todo.length) {
    const f = todo.pop()
    if (seen.has(f)) continue
    seen.add(f)
    todo.push(...staticImports(f))
  }
  return seen
}
const files = readdirSync(assets).filter((f) => f.endsWith('.js'))
const eager = closure([...readFileSync(join(dist, 'index.html'), 'utf8').matchAll(/assets\/([\w.-]+\.js)/g)].map((m) => m[1]))
const viewRoots = files.filter((f) => /^MarkdownView-.*\.js$/.test(f))
if (!viewRoots.length) bad.push(`${assets}: no MarkdownView-*.js chunk`)
const view = closure(viewRoots)
const LAZY = { mermaid: /suppressErrorRendering/, 'the code highlighter': /th-code--|th-token th-/ }
for (const f of new Set([...eager, ...view]))
  for (const [name, re] of Object.entries(LAZY)) if (re.test(read(f))) bad.push(`${f}: ${name} in an eagerly loaded chunk`)
const lazyRoot = (re) => closure(files.filter((f) => re.test(read(f))))
const minus = (a, ...b) => [...a].filter((f) => !b.some((s) => s.has(f)))
const size = (list) => {
  let raw = 0
  let gz = 0
  for (const f of list) {
    const b = readFileSync(join(assets, f))
    raw += b.length
    gz += gzipSync(b).length
  }
  return `${(raw / 1024).toFixed(1)} kB (gzip ${(gz / 1024).toFixed(1)} kB, ${list.length} files)`
}
console.log(`eager JS (index.html): ${size([...eager])}`)
console.log(`Markdown renderer chunks beyond it: ${size(minus(view, eager))}`)
console.log(`lazy highlighter: ${size(minus(lazyRoot(/th-token th-/), eager, view))}, plus one grammar chunk per language`)
console.log(`lazy mermaid: ${size(minus(lazyRoot(LAZY.mermaid), eager, view))}, plus its per-diagram-type chunks`)

if (bad.length) {
  console.error(bad.join('\n'))
  process.exit(1)
}
console.log('dist ok')
