import { Badge, Button, Card, Drawer, Group, NumberInput, SegmentedControl, SimpleGrid, Stack, Switch, Table, Tabs, Text, Textarea } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { api, ifMatch, unwrap, type XSpacePeer, type XSpaceRequest } from '@/api/client'
import { useMe, useXSpacePeers, useXSpaceRequests, useXSpaceSettings } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { accepts, disclosureLabel, sends } from '@/features/xspace/access'
import { PeerModal, RequestDetail, RequestTable } from '@/features/xspace/XSpace'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/cross-space')({ component: CrossSpacePage })

/**
 * Cross-space collaboration of a space (SM-XSPACE-*, ADR-0038): the requests it sent and received, and what it accepts
 * from and may send to each peer space.
 */
function CrossSpacePage() {
  const { spaceId } = Route.useParams()
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const [tab, setTab] = useState<string | null>('requests')
  return (
    <>
      <SectionHeader title="Cross-space" description="Requests between this space's control and other spaces, and who may send them" />
      <Tabs value={tab} onChange={setTab}>
        <Tabs.List mb="sm">
          <Tabs.Tab value="requests">Requests</Tabs.Tab>
          <Tabs.Tab value="access">Cross-space access</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="requests"><Requests spaceId={spaceId} canWrite={canWrite} /></Tabs.Panel>
        <Tabs.Panel value="access"><Access spaceId={spaceId} canWrite={canWrite} /></Tabs.Panel>
      </Tabs>
    </>
  )
}

function Requests({ spaceId, canWrite }: { spaceId: string; canWrite: boolean }) {
  const reqs = useXSpaceRequests(spaceId)
  const [dir, setDir] = useState('all')
  const [openOnly, setOpenOnly] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const items = (reqs.data ?? []).filter((r) => (dir === 'all' || r.side === dir) && (!openOnly || r.state === 'pending' || r.state === 'accepted'))
  const current: XSpaceRequest | undefined = (reqs.data ?? []).find((r) => r.id === open)
  return (
    <>
      <ErrorAlert error={reqs.error} />
      <Group justify="space-between" mb="sm">
        <SegmentedControl value={dir} onChange={setDir} data={[{ value: 'all', label: 'All' }, { value: 'incoming', label: 'Incoming' }, { value: 'outgoing', label: 'Outgoing' }]} />
        <Switch label="Open only" checked={openOnly} onChange={(e) => setOpenOnly(e.currentTarget.checked)} />
      </Group>
      <Card p={0}><RequestTable items={items} onOpen={(r) => setOpen(r.id)} /></Card>
      <Drawer opened={!!current} onClose={() => setOpen(null)} position="right" size="lg" title="Cross-space request">
        {current && <RequestDetail spaceId={spaceId} r={current} canWrite={canWrite} onDone={() => setOpen(null)} />}
      </Drawer>
    </>
  )
}

function Access({ spaceId, canWrite }: { spaceId: string; canWrite: boolean }) {
  const qc = useQueryClient()
  const peers = useXSpacePeers(spaceId)
  const settings = useXSpaceSettings(spaceId)
  const [editing, setEditing] = useState<XSpacePeer | 'new' | null>(null)
  const form = useForm({
    defaultValues: { profile: '', max_depth: 3, max_outgoing_per_hour: 60 },
    onSubmit: async ({ value }) => {
      try {
        const header = settings.data && settings.data.version > 0 ? { 'If-Match': ifMatch(settings.data.version) } : undefined
        unwrap(await api.PUT('/spaces/{space_id}/xspace/settings', { params: { path: { space_id: spaceId }, header }, body: value }))
        void qc.invalidateQueries({ queryKey: ['xspace-settings', spaceId] })
        notifyOk('Cross-space settings saved')
      } catch (e) {
        notifyError(e)
      }
    },
  })
  useEffect(() => {
    if (settings.data) form.reset({ profile: settings.data.profile, max_depth: settings.data.max_depth, max_outgoing_per_hour: settings.data.max_outgoing_per_hour })
  }, [settings.data, form])
  const remove = async (p: XSpacePeer) => {
    if (!window.confirm(`Remove this space's configuration for ${p.peer.name || p.peer.id}? Nothing is allowed with it afterwards.`)) return
    try {
      const res = await api.DELETE('/spaces/{space_id}/xspace/peers/{peer_id}', { params: { path: { space_id: spaceId, peer_id: p.peer.id } } })
      if (!res.response.ok) throw new Error(`HTTP ${res.response.status}`)
      void qc.invalidateQueries({ queryKey: ['xspace-peers', spaceId] })
    } catch (e) {
      notifyError(e)
    }
  }
  const list = peers.data ?? []
  return (
    <Stack gap="md">
      <ErrorAlert error={peers.error ?? settings.error} />
      <Card withBorder>
        <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
          <Text fw={600} mb="xs">This space</Text>
          <form.Field name="profile">
            {(f) => <Textarea label="Profile for peers" description="What spaces allowed to discover this one see about it; agents read it, so write it for them" autosize minRows={2}
              maxLength={2000} disabled={!canWrite} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <SimpleGrid cols={{ base: 1, sm: 2 }} mt="xs">
            <form.Field name="max_depth">
              {(f) => <NumberInput label="Deepest request chain accepted" description="A request asked because of a request… counts one deeper" min={1} max={8}
                disabled={!canWrite} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 3)} />}
            </form.Field>
            <form.Field name="max_outgoing_per_hour">
              {(f) => <NumberInput label="Outgoing requests per hour" description="Everything this space's control sends, to every peer" min={1} max={1000}
                disabled={!canWrite} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 60)} />}
            </form.Field>
          </SimpleGrid>
          {canWrite && (
            <Group justify="flex-end" mt="sm">
              <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save</Button>}</form.Subscribe>
            </Group>
          )}
        </form>
      </Card>
      <Card p={0}>
        <Group justify="space-between" p="sm">
          <Text fw={600}>Peer spaces</Text>
          {canWrite && <Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add peer</Button>}
        </Group>
        <Table highlightOnHover>
          <Table.Thead>
            <Table.Tr><Table.Th>Peer</Table.Th><Table.Th>Accepts from it</Table.Th><Table.Th>Replies disclose</Table.Th><Table.Th>Limits</Table.Th><Table.Th>May send it</Table.Th><Table.Th /></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {list.map((p) => (
              <Table.Tr key={p.peer.id} style={{ cursor: canWrite ? 'pointer' : undefined }} onClick={() => canWrite && setEditing(p)}>
                <Table.Td><Text size="sm" fw={500}>{p.peer.name || p.peer.id}</Text><Text size="xs" c="dimmed" className="mono">{p.peer.slug}</Text></Table.Td>
                <Table.Td><Flags items={accepts(p)} /></Table.Td>
                <Table.Td><Text size="sm">{disclosureLabel[p.inbound.disclosure]}</Text></Table.Td>
                <Table.Td><Text size="xs" c="dimmed">{p.inbound.max_per_hour}/hour · {p.inbound.max_pending} open</Text></Table.Td>
                <Table.Td><Flags items={sends(p)} /></Table.Td>
                <Table.Td>{canWrite && <Button size="xs" variant="subtle" color="owlRed" onClick={(e) => { e.stopPropagation(); void remove(p) }}>Remove</Button>}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {peers.isSuccess && list.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">No peers. Other spaces cannot see or ask this space, and its control cannot ask them.</Text>
        )}
      </Card>
      {editing && <PeerModal spaceId={spaceId} peer={editing === 'new' ? null : editing} configured={list.map((p) => p.peer.id)} onClose={() => setEditing(null)} />}
    </Stack>
  )
}

function Flags({ items }: { items: string[] }) {
  if (items.length === 0) return <Text size="sm" c="dimmed">nothing</Text>
  return <Group gap={4}>{items.map((x) => <Badge key={x} variant="light" size="sm">{x}</Badge>)}</Group>
}
