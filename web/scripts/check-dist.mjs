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
// The terminal's WebAssembly module is a hashed asset, fetched under the CSP's connect-src 'self' (ADR-0076).
if (!readdirSync(join(dist, 'assets')).some((f) => /^ghostty-vt-.*\.wasm$/.test(f))) bad.push(`${dist}/assets: no ghostty-vt-*.wasm`)
if (bad.length) {
  console.error(bad.join('\n'))
  process.exit(1)
}
console.log('dist ok')
