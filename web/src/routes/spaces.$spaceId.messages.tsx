import { Badge, Button, Card, Checkbox, Drawer, Group, Modal, SegmentedControl, Select, Stack, Text, TextInput } from '@mantine/core'
import { IconArrowRight, IconMessagePlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap } from '@/api/client'
import { keys, useMe, useMessages, useRuns, useScheduledMessages } from '@/api/queries'
import { Composer } from '@/components/composer/Composer'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { MessageDetail, party, stateColor } from '@/features/messages/MessageDetail'
import { isSystem, messagesInView, pendingSchedule, type MessageView } from '@/features/messages/scheduled'
import { ScheduledBadge } from '@/features/messages/ScheduledParts'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { atLeast, roleIn } from '@/lib/access'
import { ago } from '@/lib/format'
import { required } from '@/lib/form'

export const Route = createFileRoute('/spaces/$spaceId/messages')({
  component: MessagesPage,
  // ?view=system opens SpaceMan's own notices (a run's dialog notice is one); anything else is the default view. The key is
  // set to undefined rather than left out: the router keeps unvalidated search params, so a stray value would get through.
  validateSearch: (s: Record<string, unknown>): { view?: 'system' } => ({ view: s.view === 'system' ? 'system' : undefined }),
})

/** Durable messages between agents, control and operators (SM-COMM-001). Delivery state is SpaceMan's, never terminal text. */
function MessagesPage() {
  const { spaceId } = Route.useParams()
  const list = useMessages(spaceId)
  const pending = useScheduledMessages(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [composing, setComposing] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [onlyMine, setOnlyMine] = useState(false)
  // People and agents by default: SpaceMan's own notifications to control would otherwise bury them (UX review).
  const [view, setView] = useState<MessageView>(Route.useSearch().view ?? 'people')
  const all = list.data?.items ?? []
  const systemCount = all.filter(isSystem).length
  const scheduled = pending.data ?? []
  const items = messagesInView(view, all, scheduled, onlyMine)
  const current = all.find((m) => m.id === open) ?? scheduled.find((m) => m.id === open) ?? null
  const shown = view === 'scheduled' ? pending : list
  return (
    <>
      <SectionHeader title="Messages" description="Messages between agents, control and operators"
        actions={canWrite && <Button leftSection={<IconMessagePlus size={14} />} onClick={() => setComposing(true)}>Send message</Button>} />
      <ErrorAlert error={list.error ?? pending.error} />
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
            {/* The row wraps on a phone, so the subject keeps a readable width instead of shrinking to nothing. */}
            <Group gap="xs">
              <Badge variant="light" color={m.kind === 'request' ? 'owlYellow' : m.kind === 'notification' ? 'gray' : 'owlBlue'}>{m.kind}</Badge>
              <Text size="xs" c="dimmed" w={260} truncate>{party(m.from_kind, m.from_id, m.from_label)} <IconArrowRight size={10} /> {party(m.to_kind, m.to_id)}</Text>
              <Text size="sm" style={{ flex: '1 1 200px', minWidth: 0 }} truncate>{m.subject}</Text>
              {pendingSchedule(m)
                ? <ScheduledBadge m={m} />
                : <Text size="xs" c="dimmed" w={70} ta="right">{ago(m.created_at)}</Text>}
              <Badge color={stateColor[m.state]} variant="light">{pendingSchedule(m) ? 'scheduled' : m.state}</Badge>
            </Group>
          </Card>
        ))}
        {shown.isSuccess && items.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">
            {view === 'scheduled' ? 'No scheduled messages. Control schedules reminders to itself with spaceman_message_send.' : all.length === 0 ? 'No messages yet.' : view === 'people' ? `No messages between people and agents. ${systemCount} system notifications are under System.` : 'Nothing here.'}
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
