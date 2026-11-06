import { Alert, Badge, Button, Card, Checkbox, Code, CopyButton, Group, Menu, Modal, SegmentedControl, Select, SimpleGrid, Stack, Switch, Table, Tabs, Text, Textarea, TextInput, Title, Tooltip } from '@mantine/core'
import { IconChevronDown, IconCopy, IconTrash } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, ifMatch, unwrap, type Cursor, type Delivery, type Subscription } from '@/api/client'
import { keys, useDeliveries, useMe, useQuarantine, useReplays, useSubscription } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { coreExplanation, DeliveryBadge, OutcomeBadge, SubscriptionState, whilePausedOptions } from '@/features/events/badges'
import { DefinitionView } from '@/features/events/DefinitionView'
import { atLeast, roleIn } from '@/lib/access'
import { fieldError, required } from '@/lib/form'
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
  const sub = useSubscription(spaceId, subscriptionId)
  const deliveries = useDeliveries(spaceId, subscriptionId)
  const quarantine = useQuarantine(spaceId, subscriptionId)
  const replays = useReplays(spaceId, subscriptionId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [cursorOpen, setCursorOpen] = useState(false)
  if (sub.error) return <ErrorAlert error={sub.error} />
  if (!sub.data) return null
  const s = sub.data
  const core = s.delivery === 'core'

  const update = async (body: { enabled: boolean; start?: Cursor; while_paused?: Subscription['while_paused'] }, msg: string) => {
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
      <AnchorLink to="/spaces/$spaceId/events" params={{ spaceId }} size="sm">← All subscriptions</AnchorLink>
      <Group justify="space-between" align="flex-start">
        <Stack gap={2}>
          <Group gap="xs"><Title order={3}>{s.name}</Title><SubscriptionState sub={s} /><DeliveryBadge sub={s} /></Group>
          <Text size="sm" c="dimmed">{s.template ? `${s.template.template_slug} v${s.template.version}${s.follow === 'latest' ? ' (following new versions)' : ''}` : 'Custom definition'}
            {s.last_event_at ? ` · last event ${ago(s.last_event_at)}` : ''}</Text>
        </Stack>
        {canWrite && (
          <Group gap="xs">
            <Switch label="Enabled" checked={s.enabled} onChange={(e) => void update({ enabled: e.currentTarget.checked }, e.currentTarget.checked ? 'Enabled' : 'Disabled')} />
            {core ? (
              <Tooltip label="Core NATS keeps no history: there is nothing to replay and no cursor to move." multiline w={280}>
                <Button variant="default" disabled rightSection={<IconChevronDown size={12} />}>Cursor</Button>
              </Tooltip>
            ) : (
              <Menu position="bottom-end">
                <Menu.Target><Button variant="default" rightSection={<IconChevronDown size={12} />}>Cursor</Button></Menu.Target>
                <Menu.Dropdown>
                  <Menu.Item onClick={() => void update({ enabled: s.enabled, start: { policy: 'new', replay: false, generation: 0 } }, 'Starting from new events')}>Start now (skip anything pending)</Menu.Item>
                  <Menu.Item onClick={() => setCursorOpen(true)}>Replay…</Menu.Item>
                </Menu.Dropdown>
              </Menu>
            )}
            <Button variant="light" color="owlRed" leftSection={<IconTrash size={14} />} onClick={() => void remove()}>Delete</Button>
          </Group>
        )}
      </Group>
      {s.last_error && <ErrorAlert error={new Error(s.last_error)} title="Last error" />}
      {core && (
        <Alert color="owlYellow" title="Non-durable subscription (core NATS)">
          {coreExplanation} Messages are deduplicated by their Nats-Msg-Id header or, without one, by a hash of their subject, type, source, time and payload.
        </Alert>
      )}
      <Card>
        <SimpleGrid cols={{ base: 1, md: 2 }}>
          <Info label="Subject" value={s.subject} />
          <Info label="Variables" value={Object.entries(s.vars).map(([k, v]) => `${k}=${v}`).join('  ')} />
          {core ? <Info label="Delivery" value="core NATS subscription (no durable consumer)" /> : <Info label="Durable / consumer" value={`${s.durable}  (${s.consumer})`} />}
          <Info label="Pulse grant to add" value={s.grant} copy />
        </SimpleGrid>
        <Text size="xs" c="dimmed" mt="xs">{core ? 'No cursor: core NATS delivers each message once, as it arrives.'
          : <>Cursor: {s.cursor.policy}{s.cursor.replay ? ' (replay)' : ''}, generation {s.cursor.generation}{s.cursor_pending ? ' — waiting to be applied' : ''}</>}</Text>
        <Select mt="sm" maw={420} label="While the space is paused" allowDeselect={false} disabled={!canWrite} value={s.while_paused}
          description={whilePausedOptions.find((o) => o.value === s.while_paused)?.description}
          onChange={(v) => { if (v && v !== s.while_paused) void update({ enabled: s.enabled, while_paused: v as Subscription['while_paused'] }, 'Paused-space policy saved') }}
          data={whilePausedOptions.map((o) => ({ value: o.value, label: o.label }))} />
      </Card>
      <Tabs defaultValue="deliveries">
        <Tabs.List>
          <Tabs.Tab value="deliveries">Deliveries</Tabs.Tab>
          <Tabs.Tab value="quarantine">Quarantine {s.quarantined > 0 && <Badge ml={4} size="xs" color="owlRed">{s.quarantined}</Badge>}</Tabs.Tab>
          <Tabs.Tab value="replays">Replays {(replays.data ?? []).length > 0 && <Badge ml={4} size="xs" color="gray">{(replays.data ?? []).length}</Badge>}</Tabs.Tab>
          {s.custom && <Tabs.Tab value="definition">Definition</Tabs.Tab>}
        </Tabs.List>
        <Tabs.Panel value="deliveries" pt="sm">
          <Card p={0}>
            <Table>
              <Table.Thead><Table.Tr><Table.Th>Time</Table.Th><Table.Th>Event</Table.Th><Table.Th>Type</Table.Th><Table.Th>Origin</Table.Th><Table.Th>Outcome</Table.Th><Table.Th>Item / detail</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(deliveries.data ?? []).map((d) => (
                  <Table.Tr key={d.id}>
                    <Table.Td><Text size="xs">{datetime(d.at)}</Text></Table.Td>
                    <Table.Td><Text size="xs" className="mono">{d.event_id}</Text>{d.stream_seq > 0 && <Text size="xs" c="dimmed">seq {d.stream_seq}</Text>}</Table.Td>
                    <Table.Td><Text size="xs" className="mono">{d.type}</Text></Table.Td>
                    <Table.Td><OriginBadge d={d} /></Table.Td>
                    <Table.Td><OutcomeBadge outcome={d.outcome} /></Table.Td>
                    <Table.Td>
                      {d.inbox_item_id ? <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId: d.inbox_item_id }} size="xs">open item</AnchorLink> : <Text size="xs" c="dimmed">{d.detail}</Text>}
                    </Table.Td>
                  </Table.Tr>
                ))}
                {deliveries.isSuccess && deliveries.data.length === 0 && <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed" ta="center" p="md">No events delivered yet.</Text></Table.Td></Table.Tr>}
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
                  {q.depth > 0 && <Text size="xs" c="dimmed">Automation depth (spaceman-depth): {q.depth}. Reprocessing ingests it at depth {q.depth + 1}.</Text>}
                  <Code block className="mono" style={{ maxHeight: 200, overflow: 'auto' }}>{q.payload}</Code>
                </Stack>
              </Card>
            ))}
            {quarantine.isSuccess && quarantine.data.length === 0 && <Text size="sm" c="dimmed">Nothing in quarantine.</Text>}
          </Stack>
        </Tabs.Panel>
        <Tabs.Panel value="replays" pt="sm">
          <Text size="xs" c="dimmed" mb="xs">{core ? 'Core NATS subscriptions cannot be replayed: nothing keeps their messages.'
            : "Every replay is an explicit, audited operation with a reason. Deliveries it causes show the origin Replay; the stream's own retries show Retry."}</Text>
          <Card p={0}>
            <Table>
              <Table.Thead><Table.Tr><Table.Th>Requested</Table.Th><Table.Th>From</Table.Th><Table.Th>Reason</Table.Th><Table.Th>Items</Table.Th><Table.Th>State</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {(replays.data ?? []).map((r) => (
                  <Table.Tr key={r.id}>
                    <Table.Td><Text size="xs">{datetime(r.requested_at)}</Text><Text size="xs" c="dimmed">{r.requested_by}</Text></Table.Td>
                    <Table.Td><Text size="xs">{r.policy === 'all' ? 'everything retained' : r.policy === 'time' ? `from ${datetime(r.time)}` : `from sequence ${r.seq}`}</Text><Text size="xs" c="dimmed" className="mono">{r.id}</Text></Table.Td>
                    <Table.Td maw={320}><Text size="xs">{r.reason}</Text></Table.Td>
                    <Table.Td><Text size="xs">{r.fresh_items ? 'fresh items' : 'deduplicated'}</Text></Table.Td>
                    <Table.Td>
                      {r.superseded ? <Badge size="sm" color="gray" variant="light">superseded</Badge>
                        : r.applied ? <Tooltip label={`Events stored before ${datetime(r.boundary)} are delivered as replayed`}><Badge size="sm" color="owlGreen" variant="light">applied</Badge></Tooltip>
                          : <Badge size="sm" color="owlYellow" variant="light">waiting</Badge>}
                    </Table.Td>
                  </Table.Tr>
                ))}
                {replays.isSuccess && replays.data.length === 0 && <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed" ta="center" p="md">Never replayed.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Card>
        </Tabs.Panel>
        {s.custom && (
          <Tabs.Panel value="definition" pt="sm">
            <Card><Stack gap="xs"><Text size="xs" c="dimmed">This subscription's own definition (not from the template catalog).</Text><DefinitionView def={s.custom} /></Stack></Card>
          </Tabs.Panel>
        )}
      </Tabs>
      {cursorOpen && <ReplayModal spaceId={spaceId} sub={s} onClose={() => setCursorOpen(false)} onDone={() => {
        void qc.invalidateQueries({ queryKey: keys.subscription(spaceId, subscriptionId) })
        void qc.invalidateQueries({ queryKey: keys.subscriptions(spaceId) })
      }} />}
    </Stack>
  )
}

/** Why a delivery happened: first delivery, the stream retrying, an operator's replay, or a reprocessed quarantine entry. */
function OriginBadge({ d }: { d: Delivery }) {
  switch (d.origin) {
    case 'redelivery':
      return <Tooltip label="The stream delivered it again on its own (a retry)"><Badge size="sm" variant="light" color="owlYellow">Retry · attempt {d.attempt}</Badge></Tooltip>
    case 'replay':
      return <Tooltip label={`Replay ${d.replay_id ?? ''}`}><Badge size="sm" variant="light" color="owlPurple">Replay</Badge></Tooltip>
    case 'reprocess':
      return <Badge size="sm" variant="light" color="owlBlue">Reprocess</Badge>
    default:
      return <Badge size="sm" variant="light" color="gray">Live</Badge>
  }
}

function ReplayModal({ spaceId, sub, onClose, onDone }: { spaceId: string; sub: Subscription; onClose: () => void; onDone: () => void }) {
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { policy: 'time' as 'all' | 'time' | 'seq', time: '', seq: '', reason: '', fresh: false },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.POST('/spaces/{space_id}/subscriptions/{subscription_id}/replay', {
          params: { path: { space_id: spaceId, subscription_id: sub.id }, header: { 'If-Match': ifMatch(sub.version) } },
          body: { policy: value.policy, time: value.policy === 'time' ? new Date(value.time).toISOString() : undefined, seq: value.policy === 'seq' ? Number(value.seq) : undefined,
            reason: value.reason.trim(), fresh_items: value.fresh },
        }))
        notifyOk('Replay requested')
        onDone()
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title={`Replay ${sub.name}`} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Text size="sm">The consumer is recreated at the chosen position and the events are delivered again, marked as a replay (never as retries). The replay is audited with your reason.</Text>
          <form.Field name="policy">
            {(f) => <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as typeof f.state.value)} aria-label="Replay from"
              data={[{ value: 'time', label: 'From a time' }, { value: 'seq', label: 'From a sequence' }, { value: 'all', label: 'Everything retained' }]} />}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.policy}>
            {(policy) => policy === 'time' ? (
              <form.Field name="time">{(f) => <TextInput type="datetime-local" label="From" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            ) : policy === 'seq' ? (
              <form.Field name="seq">{(f) => <TextInput label="Stream sequence" value={f.state.value} onChange={(e) => f.handleChange(e.target.value.replace(/\D/g, ''))} />}</form.Field>
            ) : null}
          </form.Subscribe>
          <form.Field name="reason" validators={{ onBlur: required('Reason') }}>
            {(f) => <Textarea label="Reason" description="Recorded in the audit log and the replay history" required autosize minRows={2} maxLength={500} value={f.state.value}
              onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="fresh">
            {(f) => <Checkbox label="Create fresh items" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)}
              description="By default an event that already produced an inbox item is recorded as a duplicate and creates nothing. With fresh items, each replayed event creates one new item (once per replay)." />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => ({ v: s.values, busy: s.isSubmitting })}>
              {({ v, busy }) => {
                const ready = v.reason.trim() !== '' && (v.policy === 'all' || (v.policy === 'time' && v.time !== '') || (v.policy === 'seq' && v.seq !== ''))
                return <Button type="submit" disabled={!ready} loading={busy}>Replay</Button>
              }}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
