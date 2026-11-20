import { MantineProvider } from '@mantine/core'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, Outlet, RouterProvider } from '@tanstack/react-router'
import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it } from 'vitest'
import type { Run } from '@/api/client'
import { RunDialogBadge } from './RunDialogBadge'

const run = (p: Partial<Run>): Run => ({ id: 'run_1', role: 'implementor', kind: 'worker', observed: 'running', space_id: 'spc_1', status: 'blocked', status_kind: 'dialog', ...p }) as unknown as Run

beforeEach(() => {
  window.matchMedia = ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null,
    dispatchEvent: () => false })) as unknown as typeof window.matchMedia
})

async function show(r: Run) {
  const root = createRootRoute({ component: () => <Outlet /> })
  const page = createRoute({ getParentRoute: () => root, path: '/', component: () => <div data-testid="page"><RunDialogBadge run={r} /></div> })
  const messages = createRoute({ getParentRoute: () => root, path: 'spaces/$spaceId/messages' })
  const router = createRouter({ routeTree: root.addChildren([page, messages]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(<MantineProvider><RouterProvider router={router} /></MantineProvider>)
  await screen.findByTestId('page')
}

describe('RunDialogBadge', () => {
  it('says blocked on dialog and links to the space messages, where the notice is', async () => {
    await show(run({}))
    const badge = screen.getByTestId('run-dialog-badge')
    expect(badge).toHaveTextContent('blocked on dialog')
    expect(badge).toHaveAttribute('href', '/spaces/spc_1/messages?view=system')
  })
  it.each([
    ['another blocked kind', { status_kind: 'question' }],
    ['a working run', { status: 'working', status_kind: undefined }],
    ['a starting run', { observed: 'starting' }],
    ['an ended run', { observed: 'exited' }],
  ])('is not shown for %s', async (_n, p) => {
    await show(run(p as Partial<Run>))
    expect(screen.queryByTestId('run-dialog-badge')).not.toBeInTheDocument()
  })
})
