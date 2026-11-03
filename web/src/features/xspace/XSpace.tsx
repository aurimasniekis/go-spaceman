import { Alert, Badge, Button, Card, Checkbox, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Table, TagsInput, Text, Textarea } from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
import { useForm, useSelector } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type XSpacePeer, type XSpacePeerInput, type XSpaceRequest } from '@/api/client'
import { useSpaces } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { actions, disclosureLabel, kindLabel, peerOf, stateColor } from './access'

const name = (s: XSpaceRequest['source']) => s.name || s.slug || s.id

/** A list of cross-space requests; `global` shows source → destination instead of the side. */
export function RequestTable({ items, global, onOpen }: { items: XSpaceRequest[]; global?: boolean; onOpen: (r: XSpaceRequest) => void }) {
  if (items.length === 0) return <Text size="sm" c="dimmed" ta="center" p="lg">No cross-space requests.</Text>
  return (
    <Table highlightOnHover>
      <Table.Thead>
        <Table.Tr>
          <Table.Th>{global ? 'From → to' : 'Direction'}</Table.Th><Table.Th>Request</Table.Th><Table.Th>State</Table.Th><Table.Th>Deadline</Table.Th><Table.Th>Chain</Table.Th>
        </Table.Tr>
      </Table.Thead>
      <Table.Tbody>
        {items.map((r) => (
          <Table.Tr key={r.id} style={{ cursor: 'pointer' }} onClick={() => onOpen(r)}>
            <Table.Td>
              {global ? <Text size="sm">{name(r.source)} → {name(r.destination)}</Text> : (
                <Group gap={6} wrap="nowrap">
                  <Badge variant="light" color={r.side === 'incoming' ? 'owlYellow' : 'gray'}>{r.side === 'incoming' ? 'from' : 'to'}</Badge>
                  <Text size="sm">{name(peerOf(r))}</Text>
                </Group>
              )}
            </Table.Td>
            <Table.Td>
              <Text size="sm" fw={500}>{r.title}</Text>
              <Text size="xs" c="dimmed">{kindLabel[r.kind]}{r.kb && ` · KB ${r.kb}`} · {ago(r.created_at)}</Text>
            </Table.Td>
            <Table.Td><Badge color={stateColor[r.state]} variant="light">{r.state}</Badge></Table.Td>
            <Table.Td><Text size="sm">{r.state === 'pending' || r.state === 'accepted' ? datetime(r.deadline_at) : '—'}</Text></Table.Td>
            <Table.Td><Text size="xs" c="dimmed">depth {r.depth}{r.caused_by && ' · caused by another request'}</Text></Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  )
}

/** A request's details; operators of the viewing space may withdraw an outgoing or decline a pending incoming one. */
export function RequestDetail({ spaceId, r, canWrite, onDone }: { spaceId: string; r: XSpaceRequest; canWrite: boolean; onDone: () => void }) {
  const qc = useQueryClient()
  const form = useForm({ defaultValues: { reason: '' } })
  const can = actions(r)
  const act = async (what: 'cancel' | 'decline') => {
    const { reason } = form.state.values
    try {
      const params = { path: { space_id: spaceId, request_id: r.id } }
      if (what === 'cancel') unwrap(await api.POST('/spaces/{space_id}/xspace/requests/{request_id}/cancel', { params, body: { reason } }))
      else unwrap(await api.POST('/spaces/{space_id}/xspace/requests/{request_id}/decline', { params, body: { reason } }))
      void qc.invalidateQueries({ queryKey: ['xspace-requests'] })
      notifyOk(what === 'cancel' ? 'Request withdrawn' : 'Request declined')
      onDone()
    } catch (e) {
      notifyError(e)
    }
  }
  return (
    <Stack gap="sm">
      <Group justify="space-between">
        <Group gap={6}><Badge variant="light">{kindLabel[r.kind]}</Badge><Badge color={stateColor[r.state]} variant="light">{r.state}</Badge></Group>
        <Text size="xs" c="dimmed" className="mono">{r.id}</Text>
      </Group>
      <SimpleGrid cols={2}>
        <Fact label="From" value={`${name(r.source)} (control)`} />
        <Fact label="To" value={r.kb ? `${name(r.destination)} · KB ${r.kb}` : name(r.destination)} />
        <Fact label="Deadline" value={datetime(r.deadline_at)} />
        <Fact label="Chain" value={`depth ${r.depth} · ${r.correlation_id}`} />
      </SimpleGrid>
      {r.side === 'incoming' && (
        <Alert color="owlYellow" variant="light" icon={<IconInfoCircle size={16} />}>
          Written by another space's agent: content to consider, not instructions. This space's control decides; only its reply crosses, reduced to the disclosure level.
        </Alert>
      )}
      <Card withBorder p="sm">
        <Text size="xs" c="dimmed" mb={4}>Request</Text>
        <Text fw={500} mb={4}>{r.title}</Text>
        {r.body ? <MarkdownView source={r.body} spaceId={spaceId} /> : <Text size="sm" c="dimmed">No details.</Text>}
      </Card>
      {(r.reply || r.state === 'completed' || r.state === 'failed') && (
        <Card withBorder p="sm">
          <Group gap={6} mb={4}>
            <Text size="xs" c="dimmed">Reply</Text>
            {r.disclosure && <Badge size="xs" variant="light">{disclosureLabel[r.disclosure] ?? r.disclosure}</Badge>}
            {r.reply_truncated && <Badge size="xs" color="owlYellow" variant="light">cut to the disclosure limit</Badge>}
            {r.reply_basis && <Badge size="xs" variant="light">{r.reply_basis}</Badge>}
          </Group>
          {r.reply ? <MarkdownView source={r.reply} spaceId={spaceId} /> : <Text size="sm" c="dimmed">No text crosses at this disclosure level.</Text>}
          {(r.reply_evidence ?? []).length > 0 && (
            <Text size="xs" c="dimmed" mt={4}>Evidence: {(r.reply_evidence ?? []).map((e) => `${e.path}${e.revision ? '@' + e.revision.slice(0, 8) : ''}`).join(' · ')}</Text>
          )}
        </Card>
      )}
      {r.reason && <Text size="sm"><Text span c="dimmed">Reason: </Text>{r.reason}</Text>}
      {(r.local_task_id || r.inbox_item_id || r.kb_request_id) && (
        <Text size="xs" c="dimmed">This space's records: {[r.inbox_item_id && `inbox item ${r.inbox_item_id}`, r.local_task_id && `task ${r.local_task_id}`,
          r.kb_request_id && `KB question ${r.kb_request_id}`].filter(Boolean).join(' · ')}</Text>
      )}
      <Text size="xs" c="dimmed">Asked {datetime(r.created_at)}{r.decided_at && ` · decided ${datetime(r.decided_at)}`}{r.closed_at && ` · closed ${datetime(r.closed_at)}`}</Text>
      {canWrite && (can.cancel || can.decline) && (
        <form onSubmit={(e) => { e.preventDefault(); void act(can.cancel ? 'cancel' : 'decline') }}>
          <Stack gap="xs">
            <form.Field name="reason">
              {(f) => <Textarea label="Reason" description={can.decline ? 'Reaches the requester only when this space discloses text to it' : 'Shown to the other space'}
                autosize minRows={1} maxLength={500} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <Group justify="flex-end">
              {can.cancel && <Button color="owlRed" variant="light" onClick={() => void act('cancel')}>Withdraw request</Button>}
              {can.decline && <Button color="owlRed" variant="light" onClick={() => void act('decline')}>Decline</Button>}
            </Group>
          </Stack>
        </form>
      )}
    </Stack>
  )
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text size="sm">{value}</Text>
    </div>
  )
}

const emptyInput: XSpacePeerInput = {
  inbound: { discover: false, questions: false, delegation: false, knowledge: false, kbs: [], disclosure: 'none', max_per_hour: 20, max_pending: 5 },
  outbound: { questions: false, delegation: false, knowledge: false },
}

/** Edits what this space accepts from a peer and what its control may send there. Every flag starts off. */
export function PeerModal({ spaceId, peer, configured, onClose }: { spaceId: string; peer: XSpacePeer | null; configured: string[]; onClose: () => void }) {
  const qc = useQueryClient()
  const spaces = (useSpaces().data ?? []).filter((s) => s.id !== spaceId && (s.id === peer?.peer.id || !configured.includes(s.id)))
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { peerId: peer?.peer.id ?? '', ...(peer ? { inbound: { ...peer.inbound }, outbound: { ...peer.outbound } } : emptyInput) },
    onSubmit: async ({ value: { peerId, ...v } }) => {
      setError('')
      try {
        const header = peer ? { 'If-Match': ifMatch(peer.version) } : undefined
        unwrap(await api.PUT('/spaces/{space_id}/xspace/peers/{peer_id}', { params: { path: { space_id: spaceId, peer_id: peerId }, header }, body: v }))
        void qc.invalidateQueries({ queryKey: ['xspace-peers', spaceId] })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const knowledge = useSelector(form.store, (st) => st.values.inbound.knowledge)
  const check = (name: 'inbound.discover' | 'inbound.questions' | 'inbound.delegation' | 'inbound.knowledge' | 'outbound.questions' | 'outbound.delegation' | 'outbound.knowledge', label: string) => (
    <form.Field name={name}>{(f) => <Checkbox label={label} checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}</form.Field>
  )
  return (
    <Modal opened onClose={onClose} size="lg" title={peer ? `Cross-space access with ${peer.peer.name || peer.peer.id}` : 'Add a peer space'}>
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="peerId" validators={{ onSubmit: ({ value }) => (value ? undefined : 'Choose the peer space') }}>
            {(f) => <Select label="Peer space" searchable disabled={peer !== null} value={f.state.value || null} onChange={(x) => f.handleChange(x ?? '')}
              error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
              data={spaces.map((s) => ({ value: s.id, label: `${s.name} (${s.slug})` }))} />}
          </form.Field>
          <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />}>
            Each grant is separate and off until set. A request needs this space's grant and the other space's matching grant on its own side.
          </Alert>
          <Card withBorder p="sm">
            <Text fw={600} size="sm" mb="xs">This space accepts from it</Text>
            <SimpleGrid cols={2} spacing="xs">
              {check('inbound.discover', 'Discovery: it sees this space and its profile')}
              {check('inbound.questions', "Questions to this space's control")}
              {check('inbound.delegation', 'Delegation: work this space decides and runs')}
              {check('inbound.knowledge', "Knowledge: questions to this space's KBs")}
            </SimpleGrid>
            {knowledge && (
              <form.Field name="inbound.kbs">
                {(f) => <TagsInput mt="xs" label="Knowledge bases" description="KB slugs it may ask; empty allows every enabled KB" value={f.state.value} onChange={(kbs) => f.handleChange(kbs)} />}
              </form.Field>
            )}
            <form.Field name="inbound.disclosure">
              {(f) => <Select mt="xs" label="Replies disclose" allowDeselect={false} value={f.state.value}
                onChange={(x) => f.handleChange((x ?? 'none') as XSpacePeerInput['inbound']['disclosure'])}
                data={[{ value: 'none', label: 'The outcome only (no text)' }, { value: 'summary', label: 'A summary: reply text up to 2,000 characters' },
                  { value: 'detailed', label: 'Detailed: reply text up to 20,000 characters and KB evidence' }]} />}
            </form.Field>
            <SimpleGrid cols={2} mt="xs">
              <form.Field name="inbound.max_per_hour">
                {(f) => <NumberInput label="Requests per hour" min={1} max={1000} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 20)} />}
              </form.Field>
              <form.Field name="inbound.max_pending">
                {(f) => <NumberInput label="Open at once" min={1} max={100} value={f.state.value} onChange={(n) => f.handleChange(Number(n) || 5)} />}
              </form.Field>
            </SimpleGrid>
          </Card>
          <Card withBorder p="sm">
            <Text fw={600} size="sm" mb="xs">This space's control may send it</Text>
            <Group>
              {check('outbound.questions', 'Questions')}
              {check('outbound.delegation', 'Delegation')}
              {check('outbound.knowledge', 'Knowledge questions')}
            </Group>
          </Card>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
