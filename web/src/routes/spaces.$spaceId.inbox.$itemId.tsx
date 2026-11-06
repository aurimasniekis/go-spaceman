import { Badge, Button, Card, Code, Group, Menu, Modal, Stack, Text, Title } from '@mantine/core'
import { IconCheck, IconChevronDown, IconClockPause, IconRotate, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type InboxItem } from '@/api/client'
import { keys, useInboxItem, useMe } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { AttachmentList } from '@/components/AttachmentList'
import { ErrorAlert } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { InboxStatusBadge, ProvenanceBadges, sourceLabel, UrgencyBadge } from '@/features/inbox/badges'
import { TaskModal } from '@/features/queue/TaskModal'
import { atLeast, roleIn } from '@/lib/access'
import { datetime } from '@/lib/format'
import { required } from '@/lib/form'
import { notifyError, notifyOk } from '@/lib/notify'
import { useAppForm } from '@/components/form/appForm'

export const Route = createFileRoute('/spaces/$spaceId/inbox/$itemId')({ component: ItemPage })

function ItemPage() {
  const { spaceId, itemId } = Route.useParams()
  const qc = useQueryClient()
  const item = useInboxItem(spaceId, itemId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [dialog, setDialog] = useState<'reject' | 'defer' | null>(null)
  const [tasking, setTasking] = useState(false)

  if (item.error) return <ErrorAlert error={item.error} />
  if (!item.data) return null
  const it = item.data

  const setStatus = async (status: InboxItem['status'], extra: { reason?: string; defer_until?: string } = {}) => {
    try {
      const next = unwrap(await api.POST('/spaces/{space_id}/inbox/{item_id}/status', {
        params: { path: { space_id: spaceId, item_id: itemId }, header: { 'If-Match': ifMatch(it.version) } },
        body: { status, ...extra },
      }))
      qc.setQueryData(keys.inboxItem(spaceId, itemId), next)
      void qc.invalidateQueries({ queryKey: keys.inbox(spaceId) })
      notifyOk(`Marked ${next.status.replace('_', ' ')}`)
      setDialog(null)
    } catch (e) {
      notifyError(e, 'Could not change the status')
    }
  }

  return (
    <Stack gap="md">
      <AnchorLink to="/spaces/$spaceId/inbox" params={{ spaceId }} size="sm">← All inbox items</AnchorLink>
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Stack gap={4}>
          <Group gap="xs"><Title order={3}>{it.title}</Title><UrgencyBadge urgency={it.urgency} /><InboxStatusBadge status={it.status} /><ProvenanceBadges item={it} /></Group>
          <Text size="sm" c="dimmed">
            {sourceLabel[it.source_kind]}{it.source_label ? ` · ${it.source_label}` : ''} · received {datetime(it.received_at)}
            {it.source_kind === 'schedule' && typeof it.trusted?.occurrence === 'string' ? ` · occurrence ${datetime(it.trusted.occurrence)}` : ''}
            {it.status === 'deferred' && it.defer_until ? ` · deferred until ${datetime(it.defer_until)}` : ''}
          </Text>
          {it.status_reason && <Text size="sm">Reason: {it.status_reason}</Text>}
          {it.linked_kind && <Text size="sm">Associated with {it.linked_kind} <span className="mono">{it.linked_id}</span></Text>}
          {it.source_kind === 'human_response' && it.source_id && (
            <Text size="sm">The response stays on the action: <AnchorLink to="/spaces/$spaceId/human" params={{ spaceId }} search={{ action: it.source_id }} size="sm">open the human action</AnchorLink></Text>
          )}
        </Stack>
        {canWrite && (
          <Group gap="xs" wrap="nowrap">
            {it.status === 'done' || it.status === 'rejected' ? (
              <Button variant="default" leftSection={<IconRotate size={14} />} onClick={() => void setStatus('new')}>Reopen</Button>
            ) : (
              <>
                <Button variant="default" onClick={() => setTasking(true)}>Create task</Button>
                <Button leftSection={<IconCheck size={14} />} onClick={() => void setStatus('done')}>Mark done</Button>
                <Menu position="bottom-end">
                  <Menu.Target><Button variant="default" rightSection={<IconChevronDown size={12} />}>More</Button></Menu.Target>
                  <Menu.Dropdown>
                    {it.status !== 'in_progress' && <Menu.Item onClick={() => void setStatus('in_progress')}>Mark in progress</Menu.Item>}
                    <Menu.Item leftSection={<IconClockPause size={14} />} onClick={() => setDialog('defer')}>Defer…</Menu.Item>
                    <Menu.Item leftSection={<IconX size={14} />} color="owlRed" onClick={() => setDialog('reject')}>Reject…</Menu.Item>
                  </Menu.Dropdown>
                </Menu>
              </>
            )}
          </Group>
        )}
      </Group>

      <Card>
        <Group justify="space-between" mb="xs">
          <Badge variant="outline" color="gray">untrusted content</Badge>
          {it.correlation_id && <Text size="xs" c="dimmed">correlation <span className="mono">{it.correlation_id}</span></Text>}
        </Group>
        {it.content_type === 'application/json' ? (
          <Code block className="mono">{JSON.stringify(JSON.parse(it.body), null, 2)}</Code>
        ) : (
          <MarkdownView source={it.body} spaceId={spaceId} />
        )}
      </Card>
      <AttachmentList spaceId={spaceId} list={it.attachments ?? []} />
      {Object.keys(it.trusted ?? {}).length > 0 && (
        <Stack gap={4}>
          <Text size="sm" fw={600}>Source metadata</Text>
          <Text size="xs" c="dimmed">Set by the authenticated source, not by the content.</Text>
          <Code block className="mono">{JSON.stringify(it.trusted, null, 2)}</Code>
        </Stack>
      )}
      {tasking && <TaskModal spaceId={spaceId} opened onClose={() => setTasking(false)} onCreated={() => void item.refetch()}
        seed={{ title: it.title, objective: it.content_type === 'text/markdown' ? it.body : undefined, fromInboxItem: it.id }} />}
      <StatusDialog kind={dialog} onClose={() => setDialog(null)} onSubmit={(extra) => setStatus(dialog === 'defer' ? 'deferred' : 'rejected', extra)} />
    </Stack>
  )
}

function StatusDialog({ kind, onClose, onSubmit }: { kind: 'reject' | 'defer' | null; onClose: () => void; onSubmit: (v: { reason?: string; defer_until?: string }) => Promise<void> }) {
  const form = useAppForm({
    defaultValues: { reason: '', until: '' },
    onSubmit: async ({ value }) => {
      await onSubmit({ reason: value.reason || undefined, defer_until: kind === 'defer' ? new Date(value.until).toISOString() : undefined })
      form.reset()
    },
  })
  return (
    <Modal opened={kind !== null} onClose={onClose} title={kind === 'defer' ? 'Defer item' : 'Reject item'}>
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <form.AppForm>
          <Stack gap="sm">
            {kind === 'defer' && (
              <form.AppField name="until" validators={{ onBlur: required('Until') }}>
                {(f) => <f.TextField type="datetime-local" label="Until" required />}
              </form.AppField>
            )}
            <form.AppField name="reason">{(f) => <f.TextareaField label="Reason" autosize minRows={2} />}</form.AppField>
            <Group justify="flex-end">
              <Button variant="default" onClick={onClose}>Cancel</Button>
              <form.Subscribe selector={(s) => s.values.until}>
                {(until) => <form.SubmitButton color={kind === 'reject' ? 'owlRed' : undefined} disabled={kind === 'defer' && !until}>{kind === 'defer' ? 'Defer' : 'Reject'}</form.SubmitButton>}
              </form.Subscribe>
            </Group>
          </Stack>
        </form.AppForm>
      </form>
    </Modal>
  )
}
