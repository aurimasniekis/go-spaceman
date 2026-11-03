import { Alert, Badge, Button, Card, Checkbox, Group, Modal, NumberInput, Select, SimpleGrid, Stack, Table, TagsInput, Text, Textarea } from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
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
  const [reason, setReason] = useState('')
  const can = actions(r)
  const act = async (what: 'cancel' | 'decline') => {
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
        <Stack gap="xs">
          <Textarea label="Reason" description={can.decline ? 'Reaches the requester only when this space discloses text to it' : 'Shown to the other space'}
            autosize minRows={1} maxLength={500} value={reason} onChange={(e) => setReason(e.target.value)} />
          <Group justify="flex-end">
            {can.cancel && <Button color="owlRed" variant="light" onClick={() => void act('cancel')}>Withdraw request</Button>}
            {can.decline && <Button color="owlRed" variant="light" onClick={() => void act('decline')}>Decline</Button>}
          </Group>
        </Stack>
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
  const [peerId, setPeerId] = useState<string | null>(peer?.peer.id ?? null)
  const [v, setV] = useState<XSpacePeerInput>(peer ? { inbound: { ...peer.inbound }, outbound: { ...peer.outbound } } : emptyInput)
  const [error, setError] = useState('')
  const inb = (patch: Partial<XSpacePeerInput['inbound']>) => setV({ ...v, inbound: { ...v.inbound, ...patch } })
  const out = (patch: Partial<XSpacePeerInput['outbound']>) => setV({ ...v, outbound: { ...v.outbound, ...patch } })
  const save = async () => {
    if (!peerId) return setError('Choose the peer space')
    try {
      const header = peer ? { 'If-Match': ifMatch(peer.version) } : undefined
      unwrap(await api.PUT('/spaces/{space_id}/xspace/peers/{peer_id}', { params: { path: { space_id: spaceId, peer_id: peerId }, header }, body: v }))
      void qc.invalidateQueries({ queryKey: ['xspace-peers', spaceId] })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} size="lg" title={peer ? `Cross-space access with ${peer.peer.name || peer.peer.id}` : 'Add a peer space'}>
      <Stack gap="sm">
        <Select label="Peer space" searchable disabled={peer !== null} value={peerId} onChange={setPeerId}
          data={spaces.map((s) => ({ value: s.id, label: `${s.name} (${s.slug})` }))} />
        <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />}>
          Each grant is separate and off until set. A request needs this space's grant and the other space's matching grant on its own side.
        </Alert>
        <Card withBorder p="sm">
          <Text fw={600} size="sm" mb="xs">This space accepts from it</Text>
          <SimpleGrid cols={2} spacing="xs">
            <Checkbox label="Discovery: it sees this space and its profile" checked={v.inbound.discover} onChange={(e) => inb({ discover: e.currentTarget.checked })} />
            <Checkbox label="Questions to this space's control" checked={v.inbound.questions} onChange={(e) => inb({ questions: e.currentTarget.checked })} />
            <Checkbox label="Delegation: work this space decides and runs" checked={v.inbound.delegation} onChange={(e) => inb({ delegation: e.currentTarget.checked })} />
            <Checkbox label="Knowledge: questions to this space's KBs" checked={v.inbound.knowledge} onChange={(e) => inb({ knowledge: e.currentTarget.checked })} />
          </SimpleGrid>
          {v.inbound.knowledge && (
            <TagsInput mt="xs" label="Knowledge bases" description="KB slugs it may ask; empty allows every enabled KB" value={v.inbound.kbs} onChange={(kbs) => inb({ kbs })} />
          )}
          <Select mt="xs" label="Replies disclose" allowDeselect={false} value={v.inbound.disclosure}
            onChange={(x) => inb({ disclosure: (x ?? 'none') as XSpacePeerInput['inbound']['disclosure'] })}
            data={[{ value: 'none', label: 'The outcome only (no text)' }, { value: 'summary', label: 'A summary: reply text up to 2,000 characters' },
              { value: 'detailed', label: 'Detailed: reply text up to 20,000 characters and KB evidence' }]} />
          <SimpleGrid cols={2} mt="xs">
            <NumberInput label="Requests per hour" min={1} max={1000} value={v.inbound.max_per_hour} onChange={(n) => inb({ max_per_hour: Number(n) || 20 })} />
            <NumberInput label="Open at once" min={1} max={100} value={v.inbound.max_pending} onChange={(n) => inb({ max_pending: Number(n) || 5 })} />
          </SimpleGrid>
        </Card>
        <Card withBorder p="sm">
          <Text fw={600} size="sm" mb="xs">This space's control may send it</Text>
          <Group>
            <Checkbox label="Questions" checked={v.outbound.questions} onChange={(e) => out({ questions: e.currentTarget.checked })} />
            <Checkbox label="Delegation" checked={v.outbound.delegation} onChange={(e) => out({ delegation: e.currentTarget.checked })} />
            <Checkbox label="Knowledge questions" checked={v.outbound.knowledge} onChange={(e) => out({ knowledge: e.currentTarget.checked })} />
          </Group>
        </Card>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Save</Button></Group>
      </Stack>
    </Modal>
  )
}
