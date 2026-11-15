import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { api, type Task } from '@/api/client'
import { keys, runsQuery } from '@/api/queries'
import { TaskDrawer } from './TaskDrawer'
import { TaskModal } from './TaskModal'
import { taskOptions } from './taskOptions'

// jsdom has no matchMedia or ResizeObserver; Mantine's drawer, modal and combobox need them.
beforeEach(() => {
  window.matchMedia ??= ((q: string) => ({ matches: false, media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {}, onchange: null, dispatchEvent: () => false })) as unknown as typeof window.matchMedia
  globalThis.ResizeObserver ??= class { observe() {} unobserve() {} disconnect() {} } as unknown as typeof ResizeObserver
  // Nothing in these tests may reach the network: every query is seeded.
  vi.spyOn(api, 'GET').mockImplementation(() => new Promise(() => {}))
})

const SPACE = 'spc_1'
const TITLE = 'Review PR #11 (contract, ADR, helpers)'

function task(id: string, title: string, status: Task['status'], extra: Partial<Task> = {}): Task {
  return {
    id, title, status, space_id: SPACE, objective: '', acceptance: '', kind: 'ordinary', priority: 'normal', manual: false, position: 1, depth: 0,
    automation_depth: 0, preemptions: 0, exclusions: [], depends_on: [], blockers: [], created_at: '2026-11-07T00:00:00Z', updated_at: '2026-11-07T00:00:00Z',
    version: 1, ...extra,
  } as Task
}

// The space from the bug report: a cancelled and a re-filed task share a title.
const tasks = [
  task('tsk_cancelled0001', TITLE, 'cancelled'),
  task('tsk_done000000002', TITLE, 'done'),
  task('tsk_open000000003', 'Ship it', 'ready', { depends_on: ['tsk_done000000002'] }),
  task('tsk_open000000004', TITLE, 'ready'),
  task('tsk_open000000005', TITLE, 'running'),
]

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } })
  qc.setQueryData(keys.queue(SPACE), { tasks, policy: {} })
  qc.setQueryData(keys.spaceRoles(SPACE), [])
  qc.setQueryData(runsQuery(SPACE).queryKey, [])
  return render(<MantineProvider><QueryClientProvider client={qc}>{ui}</QueryClientProvider></MantineProvider>)
}

describe('task pickers with duplicate titles', () => {
  it('key options by task ID and tell duplicate titles apart', () => {
    const opts = taskOptions(tasks)
    expect(opts.map((o) => o.value)).toEqual(tasks.map((t) => t.id))
    expect(new Set(opts.map((o) => o.label)).size).toBe(opts.length)
    expect(opts.find((o) => o.value === 'tsk_open000000003')?.label).toBe('Ship it')
    expect(opts.find((o) => o.value === 'tsk_done000000002')?.label).toBe(`${TITLE} (done, …000002)`)
  })

  it('render the task drawer and its dependency picker', () => {
    wrap(<TaskDrawer spaceId={SPACE} task={tasks[2]} tasks={tasks} canWrite onClose={() => {}} />)
    expect(screen.getByRole('heading', { name: 'Ship it' })).toBeInTheDocument()
    // The selected dependency shows as the done task, not as its cancelled namesake.
    expect(screen.getAllByText(`${TITLE} (done, …000002)`).length).toBeGreaterThan(0)
  })

  it('render the new-task form with its parent and dependency pickers', () => {
    wrap(<TaskModal spaceId={SPACE} opened onClose={() => {}} />)
    expect(screen.getByRole('textbox', { name: 'Parent task' })).toBeInTheDocument()
    expect(screen.getByText('Depends on')).toBeInTheDocument()
  })
})
