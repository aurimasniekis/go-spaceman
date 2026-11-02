// Fails the build when the production bundle contains development-only code or inline scripts (the CSP forbids them).
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

const dist = new URL('../dist', import.meta.url).pathname
const bad = []
const walk = (d) => {
  for (const f of readdirSync(d)) {
    const p = join(d, f)
    if (statSync(p).isDirectory()) walk(p)
    else if (/\.(js|html)$/.test(f)) {
      const s = readFileSync(p, 'utf8')
      if (/ReactQueryDevtools|TanStackRouterDevtools/.test(s)) bad.push(`${p}: devtools in production bundle`)
      if (f.endsWith('.html') && /<script(?![^>]*\bsrc=)[^>]*>/.test(s)) bad.push(`${p}: inline <script>`)
    }
  }
}
walk(dist)
if (bad.length) {
  console.error(bad.join('\n'))
  process.exit(1)
}
console.log('dist ok')
