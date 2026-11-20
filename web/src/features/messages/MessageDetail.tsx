import { Badge, Button, Card, Group, Stack, Text } from '@mantine/core'
import { IconCheck, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type Message } from '@/api/client'
import { keys } from '@/api/queries'
import { isMiniApp } from '@/api/session'
import { AttachmentList } from '@/components/AttachmentList'
import { Composer } from '@/components/composer/Composer'
import { useAppForm } from '@/components/form/appForm'
import { MarkdownView } from '@/components/MarkdownView'
import { ago, datetime } from '@/lib/format'
import { notifyError } from '@/lib/notify'
import { pendingSchedule } from './scheduled'
import { ScheduledNote } from './ScheduledParts'

export const stateColor: Record<Message['state'], string> = { stored: 'gray', available: 'owlBlue', acknowledged: 'owlPurple', resolved: 'owlGreen' }

export function party(kind: string, id?: string, label?: string) {
  if (kind === 'operators' || kind === 'operator') return label ? `${label} (operator)` : 'operators'
  if (kind === 'control') return 'control'
  if (kind === 'system') return 'SpaceMan'
  if (kind === 'role') return `role ${id}`
  return label || id || kind
}

/** One message with its acknowledge, resolve, cancel and reply controls; shared by the space page and Needs you → Messages. */
export function MessageDetail({ spaceId, m, canWrite }: { spaceId: string; m: Message; canWrite: boolean }) {
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
      void qc.invalidateQueries({ queryKey: keys.operatorMessages })
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
      <ScheduledNote m={m} />
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
          {/* Cancelling is excluded from the Mini App (cancel-message is TelegramExcluded): it stays on the web. */}
          {pending && !isMiniApp() && (
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

