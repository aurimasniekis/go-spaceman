import { Button, Checkbox, Group, Text } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useHumanActions, useMe, useSpace } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { ActionList } from '@/features/human/ActionList'
import { isPending } from '@/features/human/badges'
import { NewActionModal } from '@/features/human/NewActionModal'
import { SpaceTypesModal } from '@/features/human/SpaceTypesModal'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'

export const Route = createFileRoute('/spaces/$spaceId/human')({ component: SpaceHumanPage })

function SpaceHumanPage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const list = useHumanActions(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [creating, setCreating] = useState(false)
  const [types, setTypes] = useState(false)
  const [showFinal, setShowFinal] = useState(false)
  const all = list.data?.items ?? []
  const items = showFinal ? all : all.filter(isPending)
  return (
    <>
      <PageHeader title={space.data?.name ?? 'Space'} description="Requests for people in this space: separate from the inbox, the queue and agent messages."
        actions={canWrite && (
          <>
            <Button variant="default" onClick={() => setTypes(true)}>Types</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New action</Button>
          </>
        )} />
      <SpaceNav spaceId={spaceId} />
      <ErrorAlert error={list.error} />
      <Group justify="space-between" mb={6}>
        <Text size="xs" c="dimmed">{all.filter(isPending).length} pending</Text>
        <Checkbox size="xs" label="Show resolved, withdrawn and expired" checked={showFinal} onChange={(e) => setShowFinal(e.currentTarget.checked)} />
      </Group>
      <ActionList actions={items} empty="No pending human actions in this space." />
      <NewActionModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)} />
      {types && <SpaceTypesModal spaceId={spaceId} onClose={() => setTypes(false)} />}
    </>
  )
}
