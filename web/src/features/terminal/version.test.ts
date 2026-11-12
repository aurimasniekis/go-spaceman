// @vitest-environment node
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'
import { PATCHED_GHOSTTY_WEB } from './ghostty'

/**
 * The terminal patches ghostty-web's private prototypes (ghostty.ts cacheViewport, skipBlankCells) and strips its
 * inlined module from the build (vite.config.ts), both written against one version. An upgrade must re-verify them.
 */
describe('ghostty-web version', () => {
  it('is the version the ADR-0076 patches were verified against', () => {
    const dir = dirname(createRequire(import.meta.url).resolve('ghostty-web/ghostty-vt.wasm'))
    const { version } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as { version: string }
    expect(version, `ghostty-web is ${version}: re-verify ADR-0076 patches (ghostty.ts cacheViewport/skipBlankCells, vite.config.ts stripGhosttyInlineWasm), then update PATCHED_GHOSTTY_WEB`).toBe(PATCHED_GHOSTTY_WEB)
    expect(__GHOSTTY_WEB_VERSION__).toBe(version)
  })
})
