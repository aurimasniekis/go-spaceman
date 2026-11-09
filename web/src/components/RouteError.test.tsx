import { MantineProvider } from '@mantine/core'
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { RouteError } from './RouteError'

beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})
afterEach(() => {
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

describe('route error screen', () => {
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
