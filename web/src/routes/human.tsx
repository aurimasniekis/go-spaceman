import { Group, SegmentedControl } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import type { HumanType } from '@/api/client'
import { useHumanPending, useSpaces } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ActionList } from '@/features/human/ActionList'
import { typeLabel } from '@/features/human/badges'

export const Route = createFileRoute('/human')({ component: HumanPage })

/** Everything waiting for a person, across the spaces you can read (SM-HUI-001). */
function HumanPage() {
  const pending = useHumanPending()
  const spaces = useSpaces()
  const [type, setType] = useState<HumanType | 'all'>('all')
  const names = Object.fromEntries((spaces.data ?? []).map((s) => [s.id, s.name]))
  const items = (pending.data?.items ?? []).filter((a) => type === 'all' || a.type === type)
  const count = (t: HumanType) => (pending.data?.items ?? []).filter((a) => a.type === t).length
  return (
    <>
      <PageHeader title="Needs you" description="Questions, approvals, to-dos and messages waiting for a person, oldest first, across your spaces." />
      <ErrorAlert error={pending.error} />
      <Group mb="sm">
        <SegmentedControl value={type} onChange={(v) => setType(v as HumanType | 'all')}
          data={[{ value: 'all', label: `All (${pending.data?.items.length ?? 0})` },
            ...(['question', 'approval', 'todo', 'message'] as const).map((t) => ({ value: t, label: `${typeLabel[t]} (${count(t)})` }))]} />
      </Group>
      <ActionList actions={items} spaceNames={names} empty="Nothing is waiting for you." />
    </>
  )
}
