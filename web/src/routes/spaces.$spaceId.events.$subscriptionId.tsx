import { Anchor, Badge, Breadcrumbs, Button, Card, Code, CopyButton, Group, Menu, Modal, SegmentedControl, SimpleGrid, Stack, Switch, Table, Tabs, Text, TextInput, Title, Tooltip } from '@mantine/core'
import { IconChevronDown, IconCopy, IconTrash } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type Cursor, type Subscription } from '@/api/client'
import { keys, useDeliveries, useMe, useQuarantine, useSpace, useSubscription } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { OutcomeBadge, SubscriptionState } from '@/features/events/badges'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/events/$subscriptionId')({ component: SubscriptionPage })

function Info({ label, value, copy }: { label: string; value: string; copy?: boolean }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Group gap={4} wrap="nowrap">
        <Code className="mono" style={{ wordBreak: 'break-all' }}>{value || '—'}</Code>
        {copy && value && <CopyButton value={value}>{({ copy: c }) => <Tooltip label="Copy"><IconCopy size={14} style={{ cursor: 'pointer' }} onClick={c} aria-label={`Copy ${label}`} /></Tooltip>}</CopyButton>}
      </Group>
    </Stack>
  )
}

function SubscriptionPage() {
  const { spaceId, subscriptionId } = Route.useParams()
  const qc = useQueryClient()
  const navigate = useNavigate()
  const space = useSpace(spaceId)
  const sub = useSubscription(spaceId, subscriptionId)
  const deliveries = useDeliveries(spaceId, subscriptionId)
  const quarantine = useQuarantine(spaceId, subscriptionId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [cursorOpen, setCursorOpen] = useState(false)
  if (sub.error) return <ErrorAlert error={sub.error} />
  if (!sub.data) return null
  const s = sub.data

  const update = async (body: { enabled: boolean; start?: Cursor }, msg: string) => {
    try {
      const next = unwrap(await api.PUT('/spaces/{space_id}/subscriptions/{subscription_id}', {
        params: { path: { space_id: spaceId, subscription_id: subscriptionId }, header: { 'If-Match': ifMatch(s.version) } }, body,
      }))
      qc.setQueryData(keys.subscription(spaceId, subscriptionId), next)
      void qc.invalidateQueries({ queryKey: keys.subscriptions(spaceId) })
      notifyOk(msg)
      setCursorOpen(false)
    } catch (e) {
      notifyError(e)
    }
  }
  const resolve = async (entry: string, action: 'reprocess' | 'discard') => {
    try {
      const r = unwrap(await api.POST('/spaces/{space_id}/subscriptions/{subscription_id}/quarantine/{entry_id}', {
        params: { path: { space_id: spaceId, subscription_id: subscriptionId, entry_id: entry } }, body: { action },
      }))
      void qc.invalidateQueries({ queryKey: keys.subscription(spaceId, subscriptionId) })
      notifyOk(r.status === 'open' ? 'Still invalid: the entry stays in quarantine' : `Event ${r.status}`)
    } catch (e) {
      notifyError(e)
    }
  }
  const remove = async () => {
    try {
      check(await api.DELETE('/spaces/{space_id}/subscriptions/{subscription_id}', { params: { path: { space_id: spaceId, subscription_id: subscriptionId } } }))
      void qc.invalidateQueries({ queryKey: keys.subscriptions(spaceId) })
      void navigate({ to: '/spaces/$spaceId/events', params: { spaceId } })
    } catch (e) {
      notifyError(e)
    }
  }

  return (
    <Stack gap="md">
      <Breadcrumbs>
        <Anchor component={Link} to="/spaces" size="sm">Spaces</Anchor>
        <AnchorLink to="/spaces/$spaceId/events" params={{ spaceId }} size="sm">{space.data?.name ?? spaceId} · Events</AnchorLink>
      </Breadcrumbs>
      <Group justify="space-between" align="flex-start">
        <Stack gap={2}>
          <Group gap="xs"><Title order={3}>{s.name}</Title><SubscriptionState sub={s} /></Group>
          <Text size="sm" c="dimmed">{s.template ? `${s.template.template_slug} v${s.template.version}${s.follow === 'latest' ? ' (following new versions)' : ''}` : 'Custom definition'}
            {s.last_event_at ? ` · last event ${ago(s.last_event_at)}` : ''}</Text>
        </Stack>
        {canWrite && (
          <Group gap="xs">
            <Switch label="Enabled" checked={s.enabled} onChange={(e) => void update({ enabled: e.currentTarget.checked }, e.currentTarget.checked ? 'Enabled' : 'Disabled')} />
            <Menu position="bottom-end">
              <Menu.Target><Button variant="default" rightSection={<IconChevronDown size={12} />}>Cursor</Button></Menu.Target>
              <Menu.Dropdown>
                <Menu.Item onClick={() => void update({ enabled: s.enabled, start: { policy: 'new', replay: false, generation: 0 } }, 'Starting from new events')}>Start now (skip anything pending)</Menu.Item>
                <Menu.Item onClick={() => setCursorOpen(true)}>Replay…</Menu.Item>
              </Menu.Dropdown>
            </Menu>
            <Button variant="light" color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => void remove()}>Delete</Button>
          </Group>
        )}
      </Group>
      {s.last_error && <ErrorAlert error={new Error(s.last_error)} title="Last error" />}
      <Card>
        <SimpleGrid cols={{ base: 1, md: 2 }}>
          <Info label="Subject" value={s.subject} />
          <Info label="Variables" value={Object.entries(s.vars).map(([k, v]) => `${k}=${v}`).join('  ')} />
          <Info label="Durable / consumer" value={`${s.durable}  (${s.consumer})`} />
          <Info label="Pulse grant to add" value={s.grant} copy />
        </SimpleGrid>
        <Text size="xs" c="dimmed" mt="xs">Cursor: {s.cursor.policy}{s.cursor.replay ? ' (replay)' : ''}, generation {s.cursor.generation}{s.cursor_pending ? ' — waiting to be applied' : ''}</Text>
      </Card>
      <Tabs defaultValue="deliveries">
        <Tabs.List>
          <Tabs.Tab value="deliveries">Deliveries</Tabs.Tab>
          <Tabs.Tab value="quarantine">Quarantine {s.quarantined > 0 && <Badge ml={4} size="xs" color="owlRed">{s.quarantined}</Badge>}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="deliveries" pt="sm">
          <Card p={0}>
            <Table>
              <Table.Thead><Table.Tr><Table.Th>Time</Table.Th><Table.Th>Event</Table.Th><Table.Th>Type</Table.Th><Table.Th>Outcome</Table.Th><Table.Th>Item / detail</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(deliveries.data ?? []).map((d) => (
                  <Table.Tr key={d.id}>
                    <Table.Td><Text size="xs">{datetime(d.at)}</Text></Table.Td>
                    <Table.Td><Text size="xs" className="mono">{d.event_id}</Text>{d.stream_seq > 0 && <Text size="xs" c="dimmed">seq {d.stream_seq}</Text>}</Table.Td>
                    <Table.Td><Text size="xs" className="mono">{d.type}</Text></Table.Td>
                    <Table.Td><OutcomeBadge outcome={d.outcome} /></Table.Td>
                    <Table.Td>
                      {d.inbox_item_id ? <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId: d.inbox_item_id }} size="xs">open item</AnchorLink> : <Text size="xs" c="dimmed">{d.detail}</Text>}
                    </Table.Td>
                  </Table.Tr>
                ))}
                {deliveries.isSuccess && deliveries.data.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed" ta="center" p="md">No events delivered yet.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Card>
        </Tabs.Panel>
        <Tabs.Panel value="quarantine" pt="sm">
          <Stack gap="sm">
            {(quarantine.data ?? []).map((q) => (
              <Card key={q.id}>
                <Stack gap={6}>
                  <Group justify="space-between">
                    <Group gap="xs"><Badge color={q.status === 'open' ? 'owlRed' : 'gray'}>{q.status}</Badge><Text size="xs" className="mono">{q.event_id}</Text><Text size="xs" c="dimmed">{datetime(q.created_at)}</Text></Group>
                    {canWrite && q.status === 'open' && (
                      <Group gap="xs">
                        <Button size="compact-xs" onClick={() => void resolve(q.id, 'reprocess')}>Reprocess</Button>
                        <Button size="compact-xs" variant="light" color="gray" onClick={() => void resolve(q.id, 'discard')}>Discard</Button>
                      </Group>
                    )}
                  </Group>
                  <Text size="sm" c="owlRed">{q.error}</Text>
                  <Code block className="mono" style={{ maxHeight: 200, overflow: 'auto' }}>{q.payload}</Code>
                </Stack>
              </Card>
            ))}
            {quarantine.isSuccess && quarantine.data.length === 0 && <Text size="sm" c="dimmed">Nothing in quarantine.</Text>}
          </Stack>
        </Tabs.Panel>
      </Tabs>
      <ReplayModal sub={s} opened={cursorOpen} onClose={() => setCursorOpen(false)} onSubmit={(start) => void update({ enabled: s.enabled, start }, 'Replay requested')} />
    </Stack>
  )
}

function ReplayModal({ sub, opened, onClose, onSubmit }: { sub: Subscription; opened: boolean; onClose: () => void; onSubmit: (c: Cursor) => void }) {
  const [policy, setPolicy] = useState<'all' | 'time' | 'seq'>('time')
  const [time, setTime] = useState('')
  const [seq, setSeq] = useState('')
  const ready = policy === 'all' || (policy === 'time' && time) || (policy === 'seq' && seq)
  return (
    <Modal opened={opened} onClose={onClose} title={`Replay ${sub.name}`}>
      <Stack gap="sm">
        <Text size="sm">The consumer is recreated at the chosen position and events are delivered again. Items that already exist are recorded as duplicates, never created twice. The replay is audited.</Text>
        <SegmentedControl value={policy} onChange={(v) => setPolicy(v as typeof policy)} data={[{ value: 'time', label: 'From a time' }, { value: 'seq', label: 'From a sequence' }, { value: 'all', label: 'Everything retained' }]} />
        {policy === 'time' && <TextInput type="datetime-local" label="From" value={time} onChange={(e) => setTime(e.target.value)} />}
        {policy === 'seq' && <TextInput label="Stream sequence" value={seq} onChange={(e) => setSeq(e.target.value.replace(/\D/g, ''))} />}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!ready} onClick={() => onSubmit({ policy, time: policy === 'time' ? new Date(time).toISOString() : undefined, seq: policy === 'seq' ? Number(seq) : undefined, replay: true, generation: 0 })}>Replay</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
