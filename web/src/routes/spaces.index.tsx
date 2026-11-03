import { Button, Card, Group, SegmentedControl, Table, Text, VisuallyHidden } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { useMe, useNodes, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SpaceState } from '@/components/StatusBadge'
import { SpaceFormModal } from '@/features/spaces/SpaceFormModal'
import { SpaceLifecycleMenu } from '@/features/spaces/SpaceLifecycle'
import { hasGlobal } from '@/lib/access'
import { ago } from '@/lib/format'

export const Route = createFileRoute('/spaces/')({ component: SpacesPage })

function SpacesPage() {
  const spaces = useSpaces()
  const nodes = useNodes()
  const me = useMe().data
  const navigate = useNavigate()
  const [creating, setCreating] = useState(false)
  // Archived spaces are kept out of the way but listed on request, where they can be restored or purged (SM-SPACE-002).
  const [show, setShow] = useState<'current' | 'archived' | 'all'>('current')
  const all = spaces.data ?? []
  const archived = all.filter((s) => s.state === 'archived').length
  const shown = all.filter((s) => show === 'all' || (show === 'archived') === (s.state === 'archived'))
  const nodeName = (id: string) => nodes.data?.find((n) => n.id === id)?.name ?? id

  return (
    <>
      <PageHeader
        title="Spaces"
        description="Each space is a persistent team of agents pinned to one node."
        actions={hasGlobal(me, 'admin') && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New space</Button>}
      />
      <ErrorAlert error={spaces.error} />
      <Group justify="flex-end" mb="xs">
        <SegmentedControl size="xs" aria-label="Which spaces to show" value={show} onChange={(v) => setShow(v as typeof show)}
          data={[{ value: 'current', label: 'Active and paused' }, { value: 'archived', label: `Archived (${archived})` }, { value: 'all', label: 'All' }]} />
      </Group>
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Name</Table.Th>
              <Table.Th>Slug</Table.Th>
              <Table.Th>State</Table.Th>
              <Table.Th>Node</Table.Th>
              <Table.Th>Updated</Table.Th>
              <Table.Th><VisuallyHidden>Actions</VisuallyHidden></Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {shown.map((s) => (
              <Table.Tr key={s.id}>
                <Table.Td>
                  <AnchorLink to="/spaces/$spaceId" params={{ spaceId: s.id }} fw={500} size="sm">{s.name}</AnchorLink>
                  {s.description && <Text size="xs" c="dimmed" lineClamp={1}>{s.description}</Text>}
                </Table.Td>
                <Table.Td className="mono">{s.slug}</Table.Td>
                <Table.Td><SpaceState space={s} /></Table.Td>
                <Table.Td>{nodeName(s.node_id)}</Table.Td>
                <Table.Td><Text size="xs" c="dimmed">{ago(s.updated_at)}</Text></Table.Td>
                <Table.Td ta="right"><SpaceLifecycleMenu space={s} onPurged={() => undefined} /></Table.Td>
              </Table.Tr>
            ))}
            {spaces.isSuccess && shown.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={6}>
                  <Group justify="center" p="lg"><Text size="sm" c="dimmed">{all.length === 0 ? 'No spaces you can see yet.' : show === 'archived' ? 'No archived spaces.' : 'No spaces in this view.'}</Text></Group>
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
      <SpaceFormModal opened={creating} onClose={() => setCreating(false)} onSaved={(s) => void navigate({ to: '/spaces/$spaceId', params: { spaceId: s.id } })} />
    </>
  )
}
