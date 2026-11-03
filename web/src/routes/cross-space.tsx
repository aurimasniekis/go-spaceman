import { Badge, Card, Drawer, Group, Table, Tabs, Text } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useAllXSpace, useMe, useSpaces } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { accepts, disclosureLabel, sends } from '@/features/xspace/access'
import { RequestDetail, RequestTable } from '@/features/xspace/XSpace'
import { hasGlobal } from '@/lib/access'

export const Route = createFileRoute('/cross-space')({ component: CrossSpaceAdmin })

/** Every cross-space request and peer configuration of the installation (global administrators, ADR-0038). */
function CrossSpaceAdmin() {
  const me = useMe().data
  const all = useAllXSpace(hasGlobal(me))
  const names = new Map((useSpaces().data ?? []).map((s) => [s.id, s.name]))
  const [open, setOpen] = useState<string | null>(null)
  const requests = all.data?.requests ?? []
  const current = requests.find((r) => r.id === open)
  return (
    <>
      <PageHeader title="Cross-space" description="Requests between spaces and what each space accepts from and may send to the others. Spaces manage their own side under Cross-space." />
      <ErrorAlert error={all.error} />
      <Tabs defaultValue="requests">
        <Tabs.List mb="sm">
          <Tabs.Tab value="requests">Requests</Tabs.Tab>
          <Tabs.Tab value="access">Access between spaces</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="requests"><Card p={0}><RequestTable global items={requests} onOpen={(r) => setOpen(r.id)} /></Card></Tabs.Panel>
        <Tabs.Panel value="access">
          <Card p={0}>
            <Table>
              <Table.Thead>
                <Table.Tr><Table.Th>Space</Table.Th><Table.Th>Peer</Table.Th><Table.Th>Accepts from the peer</Table.Th><Table.Th>Replies disclose</Table.Th><Table.Th>May send the peer</Table.Th></Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {(all.data?.peers ?? []).map((p) => (
                  <Table.Tr key={p.space_id + p.peer.id}>
                    <Table.Td><Text size="sm">{names.get(p.space_id) ?? p.space_id}</Text></Table.Td>
                    <Table.Td><Text size="sm">{p.peer.name || p.peer.id}</Text></Table.Td>
                    <Table.Td><Flags items={accepts(p)} /></Table.Td>
                    <Table.Td><Text size="sm">{disclosureLabel[p.inbound.disclosure]}</Text></Table.Td>
                    <Table.Td><Flags items={sends(p)} /></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
            {all.isSuccess && all.data.peers.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No space allows anything with another space.</Text>}
          </Card>
        </Tabs.Panel>
      </Tabs>
      <Drawer opened={!!current} onClose={() => setOpen(null)} position="right" size="lg" title="Cross-space request">
        {current && <RequestDetail spaceId={current.source.id} r={current} canWrite={false} onDone={() => setOpen(null)} />}
      </Drawer>
    </>
  )
}

function Flags({ items }: { items: string[] }) {
  if (items.length === 0) return <Text size="sm" c="dimmed">nothing</Text>
  return <Group gap={4}>{items.map((x) => <Badge key={x} variant="light" size="sm">{x}</Badge>)}</Group>
}
