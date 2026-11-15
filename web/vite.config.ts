import { defineConfig, type Plugin } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

// A running `spaceman serve` (development mode) for the dev server proxy.
const api = process.env.SPACEMAN_DEV_API_URL ?? 'http://127.0.0.1:8080'

// The installed ghostty-web version, for the terminal's prototype patches (ghostty.ts, ADR-0076): they are applied only
// to the version they were written against.
const ghosttyWebVersion = (JSON.parse(readFileSync(new URL('./node_modules/ghostty-web/package.json', import.meta.url), 'utf8')) as { version: string }).version

/**
 * Drops the WebAssembly module ghostty-web inlines as a base64 data: URL (560 kB of the terminal's chunk) for its own
 * Ghostty.load(). The terminal never uses it: it loads the module from its hashed asset (ghostty.ts wasmModule,
 * ADR-0076). The URL is replaced by one that fails to load, so a stray Ghostty.load() falls back to fetching
 * ./ghostty-vt.wasm. scripts/check-dist.mjs fails the build if a data:application/wasm URL is left anywhere.
 */
function stripGhosttyInlineWasm(): Plugin {
  return {
    name: 'spaceman:strip-ghostty-inline-wasm',
    apply: 'build',
    enforce: 'pre',
    transform(code, id) {
      if (!/[\\/]ghostty-web[\\/]dist[\\/]ghostty-web\.js$/.test(id)) return null
      const out = code.replace(/(["'`])data:application\/wasm;base64,[A-Za-z0-9+/=]*\1/g, '"about:blank"')
      if (out === code) this.error(`ghostty-web ${ghosttyWebVersion}: no inlined data:application/wasm URL found; re-check the transform (ADR-0076)`)
      return { code: out, map: null }
    },
  }
}

export default defineConfig({
  plugins: [stripGhosttyInlineWasm(), tanstackRouter({ target: 'react', autoCodeSplitting: true }), react()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  define: { __GHOSTTY_WEB_VERSION__: JSON.stringify(ghosttyWebVersion) },
  build: {
    // Embedded into the Go binary with -tags embedui (web/embed_ui.go).
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2500,
  },
  server: { proxy: { '/api': { target: api, changeOrigin: false } } },
  test: {
    globals: true,
    environment: 'jsdom',
    setupFiles: ['./src/test/setup.ts'],
    include: ['src/**/*.test.{ts,tsx}'],
  },
})
