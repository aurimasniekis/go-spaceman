import { afterEach, describe, expect, it, vi } from 'vitest'
import { installChunkReload, isChunkLoadError, reloadOnce, reloadPending, reloadWindowMs, resetChunkReload } from './chunkReload'

afterEach(() => {
  resetChunkReload()
  sessionStorage.clear()
  vi.restoreAllMocks()
})

describe('chunk load errors', () => {
  it('recognises every browser and Vite wording, and nothing else', () => {
    for (const m of [
      'Failed to fetch dynamically imported module: https://example.com/assets/spaces.index-abc.js',
      'error loading dynamically imported module: https://example.com/assets/terminals-abc.js',
      'Importing a module script failed.',
      'Unable to preload CSS for https://example.com/assets/composer-abc.css',
      "Failed to load module script: Expected a JavaScript module script but the server responded with a MIME type of \"text/html\". Strict MIME type checking is enforced for module scripts per HTML spec. 'text/html' is not a valid JavaScript MIME type.",
    ]) expect(isChunkLoadError(new TypeError(m)), m).toBe(true)
    expect(isChunkLoadError(Object.assign(new Error('x'), { name: 'ChunkLoadError' }))).toBe(true)
    for (const e of [new Error('Duplicate options are not supported'), new TypeError('Load failed'), null, undefined, 'Importing a module script failed.'])
      expect(isChunkLoadError(e)).toBe(false)
  })
})

describe('reloadOnce', () => {
  it('reloads once, then not again within the window, then again for a later upgrade', () => {
    const reload = vi.fn()
    const t = 1_800_000_000_000
    expect(reloadOnce(t, reload)).toBe(true)
    expect(reload).toHaveBeenCalledTimes(1)
    resetChunkReload() // the reloaded page: a fresh document, the same tab's sessionStorage
    // The reloaded page failed again: no loop, the caller shows a message.
    expect(reloadOnce(t + 5_000, reload)).toBe(false)
    expect(reloadOnce(t + reloadWindowMs - 1, reload)).toBe(false)
    expect(reload).toHaveBeenCalledTimes(1)
    // Unlike a per-message marker, a later upgrade in the same tab recovers again (Safari's message names no URL).
    expect(reloadOnce(t + reloadWindowMs, reload)).toBe(true)
    expect(reload).toHaveBeenCalledTimes(2)
  })

  it('does not reload when the marker cannot be stored', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota')
    })
    const reload = vi.fn()
    expect(reloadOnce(Date.now(), reload)).toBe(false)
    expect(reload).not.toHaveBeenCalled()
  })

  it("marks the reload pending and sets TanStack Router's marker for the error, so it does not reload again", () => {
    const reload = vi.fn()
    expect(reloadPending()).toBe(false)
    expect(reloadOnce(Date.now(), reload, new TypeError('Failed to fetch dynamically imported module: https://example.com/assets/a.js'))).toBe(true)
    expect(reloadPending()).toBe(true)
    expect(sessionStorage.getItem('tanstack_router_reload:Failed to fetch dynamically imported module: https://example.com/assets/a.js')).toBe('1')
    // A second chunk of the same page (/spaces is two lazy routes) fails with its own message while the reload is
    // pending: no second reload, from here or from TanStack Router.
    expect(reloadOnce(Date.now(), reload, new TypeError('Failed to fetch dynamically imported module: https://example.com/assets/b.js'))).toBe(true)
    expect(sessionStorage.getItem('tanstack_router_reload:Failed to fetch dynamically imported module: https://example.com/assets/b.js')).toBe('1')
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it("reloads for Vite's preload error only when a download failed, and never stops the error", () => {
    const reload = vi.fn()
    const target = new EventTarget() as unknown as Window
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
    installChunkReload(target)
    const fire = (payload: unknown) => {
      const e = Object.assign(new Event('vite:preloadError', { cancelable: true }), { payload })
      target.dispatchEvent(e)
      return e.defaultPrevented
    }
    // A chunk that downloaded but threw while it ran is a bug to show, not a reason to reload.
    expect(fire(new ReferenceError('process is not defined'))).toBe(false)
    expect(reload).not.toHaveBeenCalled()
    expect(reloadPending()).toBe(false)
    // Not stopped either way: the real error reaches the route's error screen, which shows the loader meanwhile.
    expect(fire(new Error('Unable to preload CSS for https://example.com/assets/composer-abc.css'))).toBe(false)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(reloadPending()).toBe(true)
    expect(fire(new TypeError('Importing a module script failed.'))).toBe(false)
    expect(reload).toHaveBeenCalledTimes(1) // no loop
  })
})
