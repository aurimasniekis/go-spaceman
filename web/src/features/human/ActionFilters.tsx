import { Button, Group, Select, TextInput } from '@mantine/core'
import { useDebouncedValue } from '@mantine/hooks'
import { IconFilterOff, IconSearch } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import type { HumanQuery } from '@/api/queries'
import { useView } from '@/lib/viewPrefs'

/** The filters of a human-action listing (SM-HUM-006, SM-HUI-001), kept per view in the TanStack view store. */
export interface ActionFilterState extends Record<string, unknown> {
  q: string
  type: string
  status: string
  assignee: string
  priority: string
  requester: string
  due: string
  sort: string
  space: string
}

const EMPTY: ActionFilterState = { q: '', type: '', status: '', assignee: '', priority: '', requester: '', due: '', sort: '', space: '' }

/** Filters for a view, and the server query they make. */
export function useActionFilters(view: string, defaults: Partial<ActionFilterState> = {}) {
  const [f, set, reset] = useView<ActionFilterState>(view, { ...EMPTY, ...defaults })
  const query: HumanQuery = { q: f.q, type: f.type, status: f.status, assignee: f.assignee, priority: f.priority, requester: f.requester, due: f.due,
    sort: f.sort, space: f.space }
  const active = Object.entries(f).some(([k, v]) => v !== '' && v !== (defaults as Record<string, unknown>)[k])
  return { f, set, reset, query, active }
}

const opt = (value: string, label: string) => ({ value, label })

/**
 * Search box, filters and sort for human actions. The search runs on the server over title and body; sorting is for
 * display and never changes a space's durable order (SM-HUI-004).
 */
export function ActionFilters({ filters, global, spaces, showStatus = true }: {
  filters: ReturnType<typeof useActionFilters>
  global?: boolean
  spaces?: { id: string; name: string }[]
  showStatus?: boolean
}) {
  const { f, set, reset, active } = filters
  // The search box updates the store (and the query) after a short pause in typing.
  const [text, setText] = useState(f.q)
  const [debounced] = useDebouncedValue(text, 300)
  useEffect(() => { if (debounced !== f.q) set({ q: debounced }) }, [debounced]) // only the typed text drives the store
  const [req, setReq] = useState(f.requester)
  const [reqDebounced] = useDebouncedValue(req, 300)
  useEffect(() => { if (reqDebounced !== f.requester) set({ requester: reqDebounced }) }, [reqDebounced])
  const sel = (label: string, key: keyof ActionFilterState, data: { value: string; label: string }[], w = 130) => (
    <Select size="xs" w={w} aria-label={label} placeholder={label} clearable data={data} value={(f[key] as string) || null}
      onChange={(v) => set({ [key]: v ?? '' } as Partial<ActionFilterState>)} />
  )
  return (
    <Group gap="xs" mb="sm" role="search" aria-label="Search and filter human actions">
      <TextInput size="xs" w={240} aria-label="Search title and text" placeholder="Search title and text" leftSection={<IconSearch size={14} />}
        value={text} onChange={(e) => setText(e.currentTarget.value)} />
      {global && spaces && sel('Space', 'space', spaces.map((s) => opt(s.id, s.name)), 150)}
      {sel('Type', 'type', [opt('question', 'Question'), opt('approval', 'Approval'), opt('todo', 'To-do'), opt('message', 'Message'), opt('custom', 'Custom')], 110)}
      {showStatus && sel('Status', 'status', global
        ? [opt('open', 'Open'), opt('in_progress', 'In progress')]
        : [opt('open', 'Open'), opt('in_progress', 'In progress'), opt('resolved', 'Resolved'), opt('cancelled', 'Cancelled'), opt('expired', 'Expired')], 120)}
      {sel('Assignee', 'assignee', [opt('me', 'Assigned to me'), opt('unassigned', 'Unassigned')], 140)}
      {sel('Priority', 'priority', [opt('urgent', 'Urgent'), opt('high', 'High'), opt('normal', 'Normal'), opt('low', 'Low')], 110)}
      {sel('Due', 'due', [opt('overdue', 'Overdue'), opt('due', 'Has a due date'), opt('expiring', 'Expires')], 130)}
      <TextInput size="xs" w={150} aria-label="Requester" placeholder="Requester" value={req} onChange={(e) => setReq(e.currentTarget.value)} />
      {sel('Sort', 'sort', [opt('rank', global ? 'Oldest first' : 'Queue order'), opt('priority', 'Priority'), opt('due', 'Due date'), opt('newest', 'Newest'),
        opt('oldest', 'Oldest'), opt('updated', 'Recently changed')], 150)}
      {active && <Button size="xs" variant="subtle" leftSection={<IconFilterOff size={14} />} onClick={() => { setText(''); setReq(''); reset() }}>Clear</Button>}
    </Group>
  )
}
