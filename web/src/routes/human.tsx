import { Checkbox, Group, Text } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { useHumanPendingSearch, useSpaces } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ActionFilters, useActionFilters } from '@/features/human/ActionFilters'
import { ActionList } from '@/features/human/ActionList'
import { useView } from '@/lib/viewPrefs'

export const Route = createFileRoute('/human')({ component: HumanPage })

/** Everything waiting for a person, across the spaces you can read (SM-HUI-001), searched and filtered on the server. */
function HumanPage() {
  const filters = useActionFilters('human.global')
  const pending = useHumanPendingSearch(filters.query)
  const spaces = useSpaces()
  const [view, setView] = useView('human.global.followup', { followUp: false })
  const names = Object.fromEntries((spaces.data ?? []).map((s) => [s.id, s.name]))
  const overdue = (a: { reminder_count: number; escalated_at?: string }) => a.reminder_count > 0 || !!a.escalated_at
  const all = pending.data?.items ?? []
  const items = all.filter((a) => !view.followUp || overdue(a))
  const readable = (spaces.data ?? []).filter((s) => pending.data?.counts[s.id] !== undefined || filters.f.space === s.id)
  return (
    <>
      <PageHeader title="Needs you" description="Questions, approvals, to-dos and messages waiting for a person across your spaces. Search, filter and sort them." />
      <ErrorAlert error={pending.error} />
      <ActionFilters filters={filters} global spaces={readable.length ? readable : (spaces.data ?? [])} />
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
