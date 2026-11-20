import { Badge, Button, Checkbox, Group, Tabs, Text, VisuallyHidden } from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { IconFilter } from '@tabler/icons-react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useHumanPendingSearch, useOperatorUnread, useSpaces } from '@/api/queries'
import { isMiniApp } from '@/api/session'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ActionFilters, useActionFilters } from '@/features/human/ActionFilters'
import { ActionList } from '@/features/human/ActionList'
import { OperatorMessages } from '@/features/messages/OperatorMessages'
import { useView } from '@/lib/viewPrefs'

export const Route = createFileRoute('/human')({
  component: HumanPage,
  // ?tab=messages opens the cross-space Messages tab; anything else is the actions list.
  validateSearch: (s: Record<string, unknown>): { tab?: 'messages' } => ({ tab: s.tab === 'messages' ? 'messages' : undefined }),
})

/** Needs you: the actions list and, next to it, the messages addressed to operators from every space. */
function HumanPage() {
  const tab = Route.useSearch().tab ?? 'actions'
  const navigate = useNavigate({ from: '/human' })
  const unread = useOperatorUnread().data ?? 0
  return (
    <>
      <PageHeader title="Needs you" description="Questions, approvals, to-dos and messages waiting for a person across your spaces. Search, filter and sort them." />
      <Tabs value={tab} onChange={(v) => void navigate({ search: { tab: v === 'messages' ? 'messages' : undefined } })} keepMounted={false}>
        <Tabs.List mb="sm">
          <Tabs.Tab value="actions">Actions</Tabs.Tab>
          <Tabs.Tab value="messages" rightSection={unread > 0 ? <Badge size="xs" color="owlYellow" circle={unread < 10}>{unread}<VisuallyHidden> unread</VisuallyHidden></Badge> : undefined}>Messages</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="actions"><ActionsTab /></Tabs.Panel>
        <Tabs.Panel value="messages"><OperatorMessages /></Tabs.Panel>
      </Tabs>
    </>
  )
}

/** Everything waiting for a person, across the spaces you can read (SM-HUI-001), searched and filtered on the server. */
function ActionsTab() {
  const filters = useActionFilters('human.global')
  const pending = useHumanPendingSearch(filters.query)
  const spaces = useSpaces()
  const [view, setView] = useView('human.global.followup', { followUp: false })
  // A phone has no room for the whole filter bar: in the Mini App it sits behind a Filter button (open while filters apply).
  const mini = isMiniApp()
  const [filtersOpen, filterPanel] = useDisclosure(mini && filters.active)
  const names = Object.fromEntries((spaces.data ?? []).map((s) => [s.id, s.name]))
  const overdue = (a: { reminder_count: number; escalated_at?: string }) => a.reminder_count > 0 || !!a.escalated_at
  const all = pending.data?.items ?? []
  const items = all.filter((a) => !view.followUp || overdue(a))
  const readable = (spaces.data ?? []).filter((s) => pending.data?.counts[s.id] !== undefined || filters.f.space === s.id)
  return (
    <>
      <ErrorAlert error={pending.error} />
      {mini && (
        <Group mb="xs">
          <Button size="xs" variant="default" leftSection={<IconFilter size={14} />} aria-expanded={filtersOpen} aria-controls="needs-you-filters"
            onClick={filterPanel.toggle}>
            Filter{filters.active ? ' (on)' : ''}
          </Button>
        </Group>
      )}
      {(!mini || filtersOpen) && (
        <div id="needs-you-filters">
          <ActionFilters filters={filters} global spaces={readable.length ? readable : (spaces.data ?? [])} />
        </div>
      )}
      <Group mb="sm" justify="space-between">
        <Text size="xs" c="dimmed" aria-live="polite">
          {filters.active ? `${all.length} matching of ${pending.data?.total ?? 0} pending` : `${pending.data?.total ?? 0} pending`}
        </Text>
        <Checkbox size="xs" label={`Only reminded or escalated (${all.filter(overdue).length})`} checked={view.followUp} onChange={(e) => setView({ followUp: e.currentTarget.checked })} />
      </Group>
      <ActionList actions={items} spaceNames={names} empty={filters.active ? 'Nothing matches the filters.' : 'Nothing is waiting for you.'} />
    </>
  )
}
