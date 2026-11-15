import { render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { describe, expect, it, vi } from 'vitest'

// The Mini App has no EventSource-capable cookie session: the terminal is replaced by a note, and opens no stream.
describe('TerminalView in the Mini App', () => {
  it('renders a note and does not open a stream', async () => {
    vi.resetModules()
    window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
    const es = vi.fn()
    vi.stubGlobal('EventSource', es)
    const { setMiniAppMode } = await import('@/api/session')
    setMiniAppMode(() => undefined)
    const { TerminalView } = await import('./TerminalView')
    render(
      <MantineProvider>
        <TerminalView spaceId="spc_1" runId="run_1" mode="live" />
      </MantineProvider>,
    )
    expect(screen.getByText(/not available in the Telegram app/)).toBeTruthy()
    expect(es).not.toHaveBeenCalled()
    vi.unstubAllGlobals()
  })
})
