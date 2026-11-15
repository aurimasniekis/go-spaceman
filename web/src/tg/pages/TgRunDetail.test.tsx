import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const state = vi.hoisted(() => ({ watch: undefined as boolean | undefined }))
vi.mock('@/api/queries', () => ({
  useRun: () => ({ data: { id: 'run_1', kind: 'worker', role: 'dev', harness: 'codex', state: 'running', observed: 'running', created_at: '2026-01-01T00:00:00Z' }, error: null }),
  useTelegramTerminalWatch: () => ({ data: state.watch }),
}))
vi.mock('@/features/terminal/TerminalView', () => ({ TerminalView: () => <div data-testid="the-terminal" /> }))
vi.mock('@/components/AnchorLink', () => ({ AnchorLink: ({ children }: { children: React.ReactNode }) => <a href="/">{children}</a> }))
vi.mock('@/components/StatusBadge', () => ({ RunState: () => null }))

import { TgRunDetail } from './TgRunDetail'

const show = () => render(<MantineProvider><TgRunDetail spaceId="spc_1" runId="run_1" /></MantineProvider>)

describe('TgRunDetail terminal', () => {
  beforeEach(() => {
    window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  })

  it.each([[false], [undefined]])('hides the terminal when the space has not opted in (%s)', (watch) => {
    state.watch = watch
    show()
    expect(screen.queryByTestId('the-terminal')).toBeNull()
    expect(screen.queryByTestId('tg-terminal')).toBeNull()
    expect(screen.getByText(/terminal is not available in the Telegram app/)).toBeTruthy()
  })

  it('shows the terminal when the space opted in', () => {
    state.watch = true
    show()
    expect(screen.getByTestId('the-terminal')).toBeTruthy()
    expect(screen.queryByText(/not available in the Telegram app/)).toBeNull()
  })
})
