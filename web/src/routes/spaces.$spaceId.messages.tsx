import { Badge, Button, Card, Checkbox, Drawer, Group, Modal, SegmentedControl, Select, Stack, Text, TextInput } from '@mantine/core'
import { IconArrowRight, IconCheck, IconClock, IconMessagePlus, IconX } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type Message } from '@/api/client'
import { keys, useMe, useMessages, useRuns } from '@/api/queries'
import { AttachmentList } from '@/components/AttachmentList'
import { Composer } from '@/components/composer/Composer'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { useAppForm } from '@/components/form/appForm'
import { MarkdownView } from '@/components/MarkdownView'
import { pendingSchedule } from '@/features/messages/scheduled'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { required } from '@/lib/form'
import { notifyError } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/messages')({ component: MessagesPage })

const stateColor: Record<Message['state'], string> = { stored: 'gray', available: 'owlBlue', acknowledged: 'owlPurple', resolved: 'owlGreen' }

function party(kind: string, id?: string, label?: string) {
  if (kind === 'operators' || kind === 'operator') return label ? `${label} (operator)` : 'operators'
  if (kind === 'control') return 'control'
  if (kind === 'system') return 'SpaceMan'
  if (kind === 'role') return `role ${id}`
  return label || id || kind
}

/** Durable messages between agents, control and operators (SM-COMM-001). Delivery state is SpaceMan's, never terminal text. */
function MessagesPage() {
  const { spaceId } = Route.useParams()
  const list = useMessages(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [composing, setComposing] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [onlyMine, setOnlyMine] = useState(false)
  // People and agents by default: SpaceMan's own notifications to control would otherwise bury them (UX review).
  const [view, setView] = useState<'people' | 'system' | 'scheduled' | 'all'>('people')
  const all = list.data?.items ?? []
  const isSystem = (m: (typeof all)[number]) => m.from_kind === 'system' || m.kind === 'notification'
  const systemCount = all.filter(isSystem).length
  const scheduled = all.filter((m) => pendingSchedule(m))
  const inView = (m: (typeof all)[number]) =>
    view === 'all' || (view === 'scheduled' ? pendingSchedule(m) : (view === 'system') === isSystem(m))
  const items = (view === 'scheduled' ? [...scheduled].sort((a, b) => Date.parse(a.deliver_at ?? '') - Date.parse(b.deliver_at ?? '')) : all)
    .filter((m) => inView(m) && (!onlyMine || m.to_kind === 'operators'))
  const current = all.find((m) => m.id === open) ?? null
  return (
    <>
      <SectionHeader title="Messages" description="Messages between agents, control and operators"
        actions={canWrite && <Button leftSection={<IconMessagePlus size={14} />} onClick={() => setComposing(true)}>Send message</Button>} />
      <ErrorAlert error={list.error} />
      <Group justify="space-between" mb={6}>
        <Group gap="sm">
          <SegmentedControl size="xs" value={view} onChange={(v) => setView(v as typeof view)} aria-label="Which messages"
            data={[{ value: 'people', label: 'People and agents' }, { value: 'system', label: `System (${systemCount})` },
              { value: 'scheduled', label: `Scheduled (${scheduled.length})` }, { value: 'all', label: 'All' }]} />
          <Text size="xs" c="dimmed">{list.data?.operators_pending ?? 0} waiting for operators</Text>
        </Group>
        <Checkbox size="xs" label="Only messages to operators" checked={onlyMine} onChange={(e) => setOnlyMine(e.currentTarget.checked)} />
      </Group>
      <Stack gap={4} role="list" aria-label="Messages">
        {items.map((m) => (
          <Card key={m.id} p="xs" role="listitem" tabIndex={0} aria-label={`${m.subject}, ${m.state}`} style={{ cursor: 'pointer' }}
            onClick={() => setOpen(m.id)} onKeyDown={(e) => { if (e.key === 'Enter') setOpen(m.id) }}>
            <Group gap="xs" wrap="nowrap">
              <Badge variant="light" color={m.kind === 'request' ? 'owlYellow' : m.kind === 'notification' ? 'gray' : 'owlBlue'}>{m.kind}</Badge>
              <Text size="xs" c="dimmed" w={260} truncate>{party(m.from_kind, m.from_id, m.from_label)} <IconArrowRight size={10} /> {party(m.to_kind, m.to_id)}</Text>
              <Text size="sm" style={{ flex: 1 }} truncate>{m.subject}</Text>
              {pendingSchedule(m)
                ? <Badge color="owlYellow" variant="light" leftSection={<IconClock size={10} />} title={`Scheduled for ${datetime(m.deliver_at)}`}>{ago(m.deliver_at)}</Badge>
                : <Text size="xs" c="dimmed" w={70} ta="right">{ago(m.created_at)}</Text>}
              <Badge color={stateColor[m.state]} variant="light">{pendingSchedule(m) ? 'scheduled' : m.state}</Badge>
            </Group>
          </Card>
        ))}
        {list.isSuccess && items.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">
            {all.length === 0 ? 'No messages yet.' : view === 'scheduled' ? 'No scheduled messages. Control schedules reminders to itself with spaceman_message_send.' : view === 'people' ? `No messages between people and agents. ${systemCount} system notifications are under System.` : 'Nothing here.'}
          </Text>
        )}
      </Stack>
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="lg" title="Message">
        {current && <MessageDetail spaceId={spaceId} m={current} canWrite={canWrite} />}
      </Drawer>
      {composing && <ComposeModal spaceId={spaceId} onClose={() => setComposing(false)} />}
    </>
  )
}

function MessageDetail({ spaceId, m, canWrite }: { spaceId: string; m: Message; canWrite: boolean }) {
  const qc = useQueryClient()
  const path = { space_id: spaceId, message_id: m.id }
  const [replyKey, setReplyKey] = useState(0)
  const replyForm = useAppForm({
    defaultValues: { body: '', attachmentIds: [] as string[] },
    onSubmit: async ({ value }) => act(async () => {
      unwrap(await api.POST('/spaces/{space_id}/messages/{message_id}/reply', { params: { path },
        body: { body: value.body, attachment_ids: value.attachmentIds.length ? value.attachmentIds : undefined } }))
      replyForm.reset()
      setReplyKey((k) => k + 1)
    }, 'Could not reply'),
  })
  const act = async (fn: () => Promise<unknown>, what: string) => {
    try {
      await fn()
      void qc.invalidateQueries({ queryKey: keys.messages(spaceId) })
    } catch (e) {
      notifyError(e, what)
    }
  }
  const open = m.state !== 'resolved'
  const pending = pendingSchedule(m)
  return (
    <Stack gap="sm">
      <Group gap="xs"><Badge variant="light">{m.kind}</Badge><Badge color={stateColor[m.state]} variant="light">{m.state}</Badge></Group>
      <Text fw={600} size="lg">{m.subject}</Text>
      <Text size="sm" c="dimmed">From {party(m.from_kind, m.from_id, m.from_label)} to {party(m.to_kind, m.to_id)} · {datetime(m.created_at)}</Text>
      {m.deliver_at && (
        <Text size="xs" fw={pending ? 600 : undefined} c="dimmed">
          {pending ? `Scheduled for ${datetime(m.deliver_at)} (${ago(m.deliver_at)}): control is not told before then` : `Scheduled for ${datetime(m.deliver_at)}`}
        </Text>
      )}
      <Text size="xs" c="dimmed">
        {m.available_at ? `Available ${ago(m.available_at)}${m.delivered_via ? ` (${m.delivered_via.replace('_', ' ')})` : ''}` : 'Not yet seen by a recipient'}
        {m.acknowledged_at ? ` · acknowledged ${ago(m.acknowledged_at)} by ${m.acknowledged_by}` : ''}
        {m.resolved_at ? ` · resolved ${ago(m.resolved_at)}${m.resolution ? `: ${m.resolution}` : ''}` : ''}
      </Text>
      <Card>
        {m.from_kind === 'run' && <Badge variant="outline" color="gray" mb="xs">written by an agent: untrusted content</Badge>}
        {m.body ? <MarkdownView source={m.body} spaceId={spaceId} /> : <Text size="sm" c="dimmed">No body.</Text>}
      </Card>
      {(m.attachments ?? []).length > 0 && <AttachmentList spaceId={spaceId} list={m.attachments ?? []} />}
      {canWrite && open && (
        <Group gap="xs">
          {m.to_kind === 'operators' && m.state !== 'acknowledged' && (
            <Button size="xs" leftSection={<IconCheck size={14} />}
              onClick={() => void act(() => api.POST('/spaces/{space_id}/messages/{message_id}/ack', { params: { path } }).then(unwrap), 'Could not acknowledge')}>
              Acknowledge
            </Button>
          )}
          {pending && (
            <Button size="xs" color="owlRed" variant="light" leftSection={<IconX size={14} />}
              onClick={() => void act(() => api.POST('/spaces/{space_id}/messages/{message_id}/cancel', { params: { path } }).then(unwrap), 'Could not cancel')}>
              Cancel scheduled message
            </Button>
          )}
          {!pending && <Button size="xs" variant="default"
            onClick={() => void act(() => api.POST('/spaces/{space_id}/messages/{message_id}/resolve', { params: { path }, body: { resolution: 'closed by an operator' } }).then(unwrap), 'Could not resolve')}>
            Resolve
          </Button>}
        </Group>
      )}
      {canWrite && m.from_kind === 'run' && (
        <form onSubmit={(e) => { e.preventDefault(); void replyForm.handleSubmit() }}>
          <Stack gap={4}>
            <Text size="sm" fw={500}>Reply</Text>
            <Text size="xs" c="dimmed">Markdown; delivered to the sending run. Paste, drop or attach files.</Text>
            <Composer key={replyKey} spaceId={spaceId} ariaLabel="Reply" minHeight={70}
              onChange={(v) => { replyForm.setFieldValue('body', v.markdown); replyForm.setFieldValue('attachmentIds', v.attachmentIds) }}
              onSubmit={() => void replyForm.handleSubmit()} />
            <replyForm.Subscribe selector={(st) => st.values.body.trim() !== ''}>
              {(has) => <Group justify="flex-end"><replyForm.AppForm><replyForm.SubmitButton size="xs" disabled={!has}>Send reply</replyForm.SubmitButton></replyForm.AppForm></Group>}
            </replyForm.Subscribe>
          </Stack>
        </form>
      )}
    </Stack>
  )
}

function ComposeModal({ spaceId, onClose }: { spaceId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const runs = (useRuns(spaceId).data ?? []).filter((r) => !['exited', 'failed', 'lost'].includes(r.observed))
  const roles = [...new Set(runs.map((r) => r.role))]
  const [error, setError] = useState('')
  const [key] = useState(idempotencyKey)
  const form = useForm({
    defaultValues: { to: 'control', subject: '', body: '', request: false, attachmentIds: [] as string[] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.POST('/spaces/{space_id}/messages', { params: { path: { space_id: spaceId }, header: { 'Idempotency-Key': key } },
          body: { to: value.to, subject: value.subject, body: value.body || undefined, request: value.request,
            attachment_ids: value.attachmentIds.length ? value.attachmentIds : undefined } }))
        void qc.invalidateQueries({ queryKey: keys.messages(spaceId) })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const targets = [{ value: 'control', label: 'Control' }, ...roles.map((r) => ({ value: `role:${r}`, label: `Every ${r} run` })),
    ...runs.map((r) => ({ value: `run:${r.id}`, label: `${r.role} · ${r.id}` }))]
  return (
    <Modal opened onClose={onClose} title="Send a message" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="to">
            {(f) => <Select label="To" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v)} data={targets} />}
          </form.Field>
          <form.Field name="subject" validators={{ onBlur: required('Subject') }}>
            {(f) => <TextInput label="Subject" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={f.state.meta.isTouched ? f.state.meta.errors[0] : undefined} />}
          </form.Field>
          <Stack gap={4}>
            <Text size="sm" fw={500}>Message</Text>
            <Composer spaceId={spaceId} ariaLabel="Message" minHeight={110} placeholder="Markdown; paste, drop or attach files"
              onChange={(v) => { form.setFieldValue('body', v.markdown); form.setFieldValue('attachmentIds', v.attachmentIds) }} />
          </Stack>
          <form.Field name="request">
            {(f) => <Checkbox label="Expect a reply (a request)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
          </form.Field>
          <Text size="xs" c="dimmed">The recipient is told that a message is waiting the next time its program is idle, and reads it through its SpaceMan tools.</Text>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button type="submit">Send</Button></Group>
        </Stack>
      </form>
    </Modal>
  )
}
