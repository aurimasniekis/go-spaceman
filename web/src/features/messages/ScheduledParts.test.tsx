import { MantineProvider } from '@mantine/core'
import { render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeAll, describe, expect, it } from 'vitest'
import { ScheduledBadge, ScheduledNote } from './ScheduledParts'

const now = Date.parse('2026-11-07T12:00:00Z')
beforeAll(() => {
  // jsdom has no matchMedia (Mantine reads the color scheme).
  window.matchMedia ??= ((q: string) => ({
    matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
})
const show = (ui: ReactNode) => render(<MantineProvider>{ui}</MantineProvider>)

describe('scheduled messages', () => {
  it('shows when a pending one is due', () => {
    show(<><ScheduledBadge m={{ state: 'stored', deliver_at: '2026-11-07T18:00:00Z' }} now={now} />
      <ScheduledNote m={{ state: 'stored', deliver_at: '2026-11-07T18:00:00Z' }} now={now} /></>)
    expect(screen.getByText('in 6h')).toBeInTheDocument()
    expect(screen.getByText(/^Scheduled for .*\(in 6h\): control is not told before then$/)).toBeInTheDocument()
  })
  it('keeps the time of a delivered one, without the badge', () => {
    show(<><ScheduledBadge m={{ state: 'available', deliver_at: '2026-11-07T06:00:00Z' }} now={now} />
      <ScheduledNote m={{ state: 'available', deliver_at: '2026-11-07T06:00:00Z' }} now={now} /></>)
    expect(screen.queryByText(/ago|in /)).toBeNull()
    expect(screen.getByText(/^Scheduled for [^(]+$/)).toBeInTheDocument()
  })
  it('renders nothing for an ordinary message', () => {
    const { container } = show(<><ScheduledBadge m={{ state: 'stored' }} now={now} /><ScheduledNote m={{ state: 'stored' }} now={now} /></>)
    expect(container.querySelector('.mantine-Badge-root, .mantine-Text-root')).toBeNull()
  })
})
