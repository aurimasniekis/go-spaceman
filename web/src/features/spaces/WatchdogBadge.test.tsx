import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { keys } from '@/api/queries'
import { backlogText, WatchdogBadge, watchdogBadge } from './WatchdogBadge'

type W = NonNullable<Parameters<typeof watchdogBadge>[0]>
const w = (over: Partial<W>): W =>
  ({ observed: true, status: 'idle', nudges: 0, escalated: false, backlog: { messages: 0, reviews: 0, failed: 0, exited: 0, startable: 0, free_slots: 0 }, ...over }) as W

describe('watchdog badge', () => {
  beforeEach(() => {
    window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  })

  it('says nothing unless the watchdog is nudging or has escalated', () => {
    for (const status of ['off', 'idle'] as const) expect(watchdogBadge(w({ status }))).toBeNull()
    expect(watchdogBadge(undefined)).toBeNull()
  })

  it('names the reminders and what waits, counts only', () => {
    const b = watchdogBadge(w({ status: 'nudging', nudges: 2, backlog: { messages: 2, reviews: 1, failed: 0, exited: 0, startable: 0, free_slots: 0 } }))!
    expect(b.label).toBe('being nudged')
    expect(b.tip).toContain('2 reminders sent')
    expect(b.tip).toContain('2 messages, 1 review')
  })

  it('turns red once escalated', () => {
    const b = watchdogBadge(w({ status: 'escalated', nudges: 3, escalated: true }))!
    expect(b.label).toBe('not responding')
    expect(b.color).toBe('owlRed')
    expect(backlogText(w({}).backlog)).toBe('')
  })

  it('renders on the control row from the cached state', () => {
    const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
    qc.setQueryData(keys.watchdog('spc_1'), w({ status: 'escalated', nudges: 3, escalated: true }))
    render(<MantineProvider><QueryClientProvider client={qc}><WatchdogBadge spaceId="spc_1" /></QueryClientProvider></MantineProvider>)
    expect(screen.getByTestId('watchdog-badge')).toHaveTextContent('not responding')
  })
})
