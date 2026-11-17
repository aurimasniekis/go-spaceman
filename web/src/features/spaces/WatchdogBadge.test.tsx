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

  it('words the backlog', () => {
    const b = (over: Partial<W['backlog']>) => ({ ...w({}).backlog, ...over })
    for (const [backlog, want] of [
      [b({ messages: 1 }), '1 message'],
      [b({ failed: 1 }), '1 failed run'],
      [b({ failed: 3 }), '3 failed runs'],
      [b({ exited: 1 }), '1 ended run'],
      [b({ exited: 2 }), '2 ended runs'],
      [b({ startable: 1, free_slots: 2 }), '1 task to start'],
      [b({ startable: 4, free_slots: 2 }), '4 tasks to start'],
      [b({ messages: 2, reviews: 1, failed: 1, exited: 2, startable: 3 }), '2 messages, 1 review, 1 failed run, 2 ended runs, 3 tasks to start'],
    ] as const) expect(backlogText(backlog)).toBe(want)
  })

  it('words the reminders, the last one and the tooltip', () => {
    const waiting = { messages: 0, reviews: 0, failed: 1, exited: 1, startable: 2, free_slots: 1 }
    const one = watchdogBadge(w({ status: 'nudging', nudges: 1, backlog: waiting }))!
    expect(one.tip).toBe('The watchdog reminded control of waiting work: 1 reminder sent. Waiting: 1 failed run, 1 ended run, 2 tasks to start.')
    const last = watchdogBadge(w({ status: 'nudging', nudges: 2, last_nudge: new Date(Date.now() - 5 * 60_000).toISOString() }))!
    expect(last.tip).toMatch(/^The watchdog reminded control of waiting work: 2 reminders sent, last .+\.$/)
    expect(last.tip).not.toContain('Waiting:')
    const esc = watchdogBadge(w({ status: 'escalated', nudges: 3, escalated: true, backlog: waiting }))!
    expect(esc.tip).toBe("Control left waiting work alone after 3 reminders sent; the space's people were told. Waiting: 1 failed run, 1 ended run, 2 tasks to start.")
  })

  it('renders only on the control run the state belongs to', () => {
    const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
    qc.setQueryData(keys.watchdog('spc_1'), w({ status: 'nudging', nudges: 1, control_run: 'run_new' }))
    const ui = (runId: string) => <MantineProvider><QueryClientProvider client={qc}><WatchdogBadge spaceId="spc_1" runId={runId} /></QueryClientProvider></MantineProvider>
    const { unmount } = render(ui('run_old'))
    expect(screen.queryByTestId('watchdog-badge')).toBeNull()
    unmount()
    render(ui('run_new'))
    expect(screen.getByTestId('watchdog-badge')).toHaveTextContent('being nudged')
  })

  it('renders on the control row from the cached state', () => {
    const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
    qc.setQueryData(keys.watchdog('spc_1'), w({ status: 'escalated', nudges: 3, escalated: true, control_run: 'run_1' }))
    render(<MantineProvider><QueryClientProvider client={qc}><WatchdogBadge spaceId="spc_1" runId="run_1" /></QueryClientProvider></MantineProvider>)
    expect(screen.getByTestId('watchdog-badge')).toHaveTextContent('not responding')
  })
})
