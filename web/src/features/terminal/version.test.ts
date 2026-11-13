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

  // The terminal loads the WebAssembly module of a later release than the JavaScript (ghostty.ts, ADR-0076): the module
  // must keep the API of 0.4.0's JavaScript (scrolled.test.ts and the other module tests run against it).
  it('loads the WebAssembly module verified with that JavaScript', () => {
    const dir = dirname(createRequire(import.meta.url).resolve('ghostty-web-vt/ghostty-vt.wasm'))
    const { version } = JSON.parse(readFileSync(join(dir, 'package.json'), 'utf8')) as { version: string }
    expect(version, `ghostty-web-vt is ${version}: re-verify the module with the ghostty-web ${PATCHED_GHOSTTY_WEB} JavaScript (ADR-0076)`).toBe('0.4.0-next.21.g640db46')
  })
})
