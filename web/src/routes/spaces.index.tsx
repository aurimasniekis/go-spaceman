import { Button, Card, Group, Table, Text } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { useMe, useNodes, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SpaceState } from '@/components/StatusBadge'
import { SpaceFormModal } from '@/features/spaces/SpaceFormModal'
import { hasGlobal } from '@/lib/access'
import { ago } from '@/lib/format'

export const Route = createFileRoute('/spaces/')({ component: SpacesPage })

function SpacesPage() {
  const spaces = useSpaces()
  const nodes = useNodes()
  const me = useMe().data
  const navigate = useNavigate()
  const [creating, setCreating] = useState(false)
  const nodeName = (id: string) => nodes.data?.find((n) => n.id === id)?.name ?? id

  return (
    <>
      <PageHeader
        title="Spaces"
        description="Each space is a persistent team of agents pinned to one node."
        actions={hasGlobal(me, 'admin') && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New space</Button>}
      />
      <ErrorAlert error={spaces.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Name</Table.Th>
              <Table.Th>Slug</Table.Th>
              <Table.Th>State</Table.Th>
              <Table.Th>Node</Table.Th>
              <Table.Th>Updated</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(spaces.data ?? []).map((s) => (
              <Table.Tr key={s.id}>
                <Table.Td>
                  <AnchorLink to="/spaces/$spaceId" params={{ spaceId: s.id }} fw={500} size="sm">{s.name}</AnchorLink>
                  {s.description && <Text size="xs" c="dimmed" lineClamp={1}>{s.description}</Text>}
                </Table.Td>
                <Table.Td className="mono">{s.slug}</Table.Td>
                <Table.Td><SpaceState space={s} /></Table.Td>
                <Table.Td>{nodeName(s.node_id)}</Table.Td>
                <Table.Td><Text size="xs" c="dimmed">{ago(s.updated_at)}</Text></Table.Td>
              </Table.Tr>
            ))}
            {spaces.isSuccess && spaces.data.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={5}>
                  <Group justify="center" p="lg"><Text size="sm" c="dimmed">No spaces you can see yet.</Text></Group>
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
