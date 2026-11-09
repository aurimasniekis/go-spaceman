import { afterEach, describe, expect, it, vi } from 'vitest'
import { installChunkReload, isChunkLoadError, reloadOnce, reloadWindowMs } from './chunkReload'

afterEach(() => {
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

  it("stops Vite's preload error only when it reloads", () => {
    const reload = vi.fn()
    const target = new EventTarget() as unknown as Window
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
    installChunkReload(target)
    const fire = () => {
      const e = new Event('vite:preloadError', { cancelable: true })
      target.dispatchEvent(e)
      return e.defaultPrevented
    }
    expect(fire()).toBe(true)
    expect(reload).toHaveBeenCalledTimes(1)
    expect(fire()).toBe(false) // reaches the route's error screen instead of looping
    expect(reload).toHaveBeenCalledTimes(1)
  })
})
