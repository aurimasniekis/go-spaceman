import { MantineProvider } from '@mantine/core'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, lazyRouteComponent, Outlet, RouterProvider } from '@tanstack/react-router'
import { render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { installChunkReload, resetChunkReload } from '@/lib/chunkReload'
import { RouteError } from './RouteError'

beforeAll(() => installChunkReload(window))

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})
afterEach(() => {
  resetChunkReload()
  sessionStorage.clear()
  vi.restoreAllMocks()
})

function renderFailing(error: Error) {
  const root = createRootRoute({
    component: () => {
      throw error
    },
  })
  const router = createRouter({ routeTree: root, history: createMemoryHistory({ initialEntries: ['/'] }), defaultErrorComponent: RouteError })
  return render(<MantineProvider><RouterProvider router={router} /></MantineProvider>)
}

/** Stubs window.location.reload and silences React's and the router's error logging. */
function stubReload() {
  vi.spyOn(console, 'error').mockImplementation(() => {})
  vi.spyOn(console, 'warn').mockImplementation(() => {})
  const reload = vi.fn()
  vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
  return reload
}

/**
 * A route at /page whose component is a code-split chunk, imported the way Vite's preload helper does it: a failed
 * import (a download failure or the chunk's own top-level error) first raises vite:preloadError, then rejects unless a
 * listener stopped the event, in which case it resolves with undefined.
 */
function renderChunkRoute(importError: Error) {
  const viteImport = () =>
    Promise.reject(importError).catch((err: unknown) => {
      const e = Object.assign(new Event('vite:preloadError', { cancelable: true }), { payload: err })
      window.dispatchEvent(e)
      if (!e.defaultPrevented) throw err
    }) as unknown as Promise<{ default: () => null }>
  const root = createRootRoute({ component: Outlet })
  const page = createRoute({ getParentRoute: () => root, path: '/page', component: lazyRouteComponent(viteImport) })
  const router = createRouter({
    routeTree: root.addChildren([page]),
    history: createMemoryHistory({ initialEntries: ['/page'] }),
    defaultErrorComponent: RouteError,
  })
  // Every error screen the page ever showed, not only the last one.
  const shown: string[] = []
  const observer = new MutationObserver(() => {
    for (const a of document.querySelectorAll('[role="alert"]')) if (!shown.includes(a.textContent ?? '')) shown.push(a.textContent ?? '')
  })
  observer.observe(document.body, { childList: true, subtree: true, characterData: true })
  const view = render(<MantineProvider><RouterProvider router={router} /></MantineProvider>)
  return { ...view, shown, stop: () => observer.disconnect() }
}

describe('route error screen', () => {
  it('shows only the loader, never an error, while a route chunk that failed to download reloads the page', async () => {
    const reload = stubReload()
    const { shown, stop } = renderChunkRoute(new TypeError('Failed to fetch dynamically imported module: https://example.com/assets/spaces.index-abc.js'))
    await waitFor(() => expect(reload).toHaveBeenCalled())
    // Give the router time to settle on whatever it shows next.
    await new Promise((r) => setTimeout(r, 50))
    stop()
    expect(shown).toEqual([])
    expect(screen.queryByRole('alert')).toBeNull()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it("shows a chunk's own runtime error without reloading", async () => {
    const reload = stubReload()
    const { shown, stop } = renderChunkRoute(new ReferenceError('process is not defined'))
    const alert = await screen.findByRole('alert')
    stop()
    expect(alert).toHaveTextContent('This page failed to load')
    expect(alert).toHaveTextContent('process is not defined')
    expect(shown).toHaveLength(1)
    expect(reload).not.toHaveBeenCalled()
  })

  it("reloads for Safari's message, then says SpaceMan was updated when it fails again after the reload", async () => {
    const reload = stubReload()
    // Safari and every iOS browser: the same message, with no URL, for every module that fails.
    const safari = () => new TypeError('Importing a module script failed.')
    const first = renderChunkRoute(safari())
    expect(await screen.findByRole('status', { name: 'Loading the latest version' })).toBeInTheDocument()
    expect(reload).toHaveBeenCalled()
    const reloads = reload.mock.calls.length
    first.stop()
    expect(first.shown).toEqual([])
    first.unmount()
    // The reloaded page: a fresh document (nothing pending), the same tab's sessionStorage.
    resetChunkReload()
    const second = renderChunkRoute(safari())
    const alert = await screen.findByRole('alert')
    second.stop()
    expect(alert).toHaveTextContent('SpaceMan was updated')
    expect(screen.getByRole('button', { name: 'Reload' })).toBeInTheDocument()
    expect(reload).toHaveBeenCalledTimes(reloads)
  })

  it('shows the loader for any error while a reload is pending', async () => {
    const reload = stubReload()
    window.dispatchEvent(Object.assign(new Event('vite:preloadError'), { payload: new Error('Unable to preload CSS for https://example.com/assets/composer-abc.css') }))
    expect(reload).toHaveBeenCalledTimes(1)
    renderFailing(new TypeError("Cannot read properties of undefined (reading 'component')"))
    expect(await screen.findByRole('status', { name: 'Loading the latest version' })).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('reloads once for a chunk that failed to download', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    const reload = vi.fn()
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
    renderFailing(new TypeError('Importing a module script failed.'))
    expect(await screen.findByRole('status', { name: 'Loading the latest version' })).toBeInTheDocument()
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('says SpaceMan was updated, with a Reload button, when a reload did not help', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    const reload = vi.fn()
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
    sessionStorage.setItem('spaceman:chunk-reload', String(Date.now()))
    renderFailing(new TypeError('Failed to fetch dynamically imported module: https://example.com/assets/spaces.index-abc.js'))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('SpaceMan was updated')
    expect(alert).toHaveTextContent('spaces.index-abc.js')
    expect(reload).not.toHaveBeenCalled()
    screen.getByRole('button', { name: 'Reload' }).click()
    expect(reload).toHaveBeenCalledTimes(1)
    expect(screen.queryByRole('button', { name: 'Try again' })).toBeNull()
  })

  it('shows any other error with its message and Try again, without reloading', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    vi.spyOn(console, 'warn').mockImplementation(() => {})
    const reload = vi.fn()
    vi.spyOn(window, 'location', 'get').mockReturnValue({ ...window.location, reload } as Location)
    renderFailing(new Error('Cannot read properties of undefined'))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('This page failed to load')
    expect(alert).toHaveTextContent('Cannot read properties of undefined')
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument()
    expect(reload).not.toHaveBeenCalled()
  })
})
