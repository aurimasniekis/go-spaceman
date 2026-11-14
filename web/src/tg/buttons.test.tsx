import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { act, render } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { useBackButton, useMainButton, type MainButtonSpec } from './buttons'

function button() {
  const handlers = new Set<() => void>()
  return {
    handlers,
    isVisible: false,
    show: vi.fn(),
    hide: vi.fn(),
    onClick: (cb: () => void) => handlers.add(cb),
    offClick: (cb: () => void) => handlers.delete(cb),
    setParams: vi.fn(),
    showProgress: vi.fn(),
    hideProgress: vi.fn(),
  }
}

afterEach(() => delete (window as unknown as { Telegram?: unknown }).Telegram)

describe('BackButton', () => {
  it('is hidden on the start page, shown elsewhere, and goes back in the history', async () => {
    const back = button()
    ;(window as unknown as { Telegram: unknown }).Telegram = { WebApp: { BackButton: back } }
    const Probe = () => {
      useBackButton()
      return null
    }
    const root = createRootRoute({ component: Probe })
    const home = createRoute({ getParentRoute: () => root, path: '/' })
    const page = createRoute({ getParentRoute: () => root, path: '/page' })
    const router = createRouter({ routeTree: root.addChildren([home, page]), history: createMemoryHistory({ initialEntries: ['/'] }) })
    await router.load()
    render(<RouterProvider router={router} />)
    await vi.waitFor(() => expect(back.hide).toHaveBeenCalled())
    expect(back.show).not.toHaveBeenCalled()
    await act(() => router.navigate({ to: '/page' as never }))
    await vi.waitFor(() => expect(back.show).toHaveBeenCalled())
    await act(async () => {
      for (const h of back.handlers) h()
    })
    await vi.waitFor(() => expect(router.state.location.pathname).toBe('/'))
  })
})

describe('MainButton', () => {
  const Probe = ({ spec }: { spec: MainButtonSpec | null }) => {
    useMainButton(spec)
    return null
  }
  it('shows with the page text, follows disabled and loading, clicks through, and hides on unmount', () => {
    const main = button()
    ;(window as unknown as { Telegram: unknown }).Telegram = { WebApp: { MainButton: main } }
    const onClick = vi.fn()
    const { rerender, unmount } = render(<Probe spec={{ text: 'Send', onClick }} />)
    expect(main.setParams).toHaveBeenLastCalledWith({ text: 'Send', is_active: true, is_visible: true })
    for (const h of main.handlers) h()
    expect(onClick).toHaveBeenCalledOnce()
    rerender(<Probe spec={{ text: 'Send', onClick, loading: true }} />)
    expect(main.setParams).toHaveBeenLastCalledWith({ text: 'Send', is_active: false, is_visible: true })
    expect(main.showProgress).toHaveBeenCalled()
    unmount()
    expect(main.hide).toHaveBeenCalled()
    expect(main.handlers.size).toBe(0)
  })
  it('does nothing outside Telegram or without a spec', () => {
    render(<Probe spec={{ text: 'x', onClick: () => undefined }} />)
    const main = button()
    ;(window as unknown as { Telegram: unknown }).Telegram = { WebApp: { MainButton: main } }
    render(<Probe spec={null} />)
    expect(main.setParams).not.toHaveBeenCalled()
  })
})
