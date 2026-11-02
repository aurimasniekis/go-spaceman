import { Anchor, Badge, Breadcrumbs, Button, Card, Code, Group, Image, Menu, Modal, SimpleGrid, Stack, Text, Textarea, TextInput, Title } from '@mantine/core'
import { IconCheck, IconChevronDown, IconClockPause, IconDownload, IconPaperclip, IconRotate, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Attachment, type InboxItem } from '@/api/client'
import { keys, useInboxItem, useMe, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { formatBytes } from '@/components/composer/upload'
import { ErrorAlert } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { InboxStatusBadge, sourceLabel, UrgencyBadge } from '@/features/inbox/badges'
import { TaskModal } from '@/features/queue/TaskModal'
import { atLeast, roleIn } from '@/lib/access'
import { datetime } from '@/lib/format'
import { attachmentURL } from '@/lib/markdown'
import { notifyError, notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/inbox/$itemId')({ component: ItemPage })

function Attachments({ spaceId, list }: { spaceId: string; list: Attachment[] }) {
  if (list.length === 0) return null
  return (
    <Stack gap={6}>
      <Text size="sm" fw={600}>Attachments</Text>
      <SimpleGrid cols={{ base: 2, sm: 4 }} spacing="xs">
        {list.map((a) => (
          <Card key={a.id} p="xs">
            {a.inline ? <Image src={attachmentURL(spaceId, a.id)} alt={a.name} h={90} fit="contain" radius="sm" /> : <IconPaperclip size={24} />}
            <Group justify="space-between" wrap="nowrap" mt={4}>
              <Text size="xs" truncate title={a.name}>{a.name}</Text>
              <Anchor href={attachmentURL(spaceId, a.id)} download={a.name} aria-label={`Download ${a.name}`}><IconDownload size={14} /></Anchor>
            </Group>
            <Text size="xs" c="dimmed">{formatBytes(a.size)} · {a.mime}</Text>
          </Card>
        ))}
      </SimpleGrid>
    </Stack>
  )
}

function ItemPage() {
  const { spaceId, itemId } = Route.useParams()
  const qc = useQueryClient()
  const space = useSpace(spaceId)
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
      <Breadcrumbs>
        <Anchor component={Link} to="/spaces" size="sm">Spaces</Anchor>
        <AnchorLink to="/spaces/$spaceId/inbox" params={{ spaceId }} size="sm">{space.data?.name ?? spaceId} · Inbox</AnchorLink>
      </Breadcrumbs>
      <Group justify="space-between" align="flex-start" wrap="nowrap">
        <Stack gap={4}>
          <Group gap="xs"><Title order={3}>{it.title}</Title><UrgencyBadge urgency={it.urgency} /><InboxStatusBadge status={it.status} /></Group>
          <Text size="sm" c="dimmed">
            {sourceLabel[it.source_kind]}{it.source_label ? ` · ${it.source_label}` : ''} · received {datetime(it.received_at)}
            {it.status === 'deferred' && it.defer_until ? ` · deferred until ${datetime(it.defer_until)}` : ''}
          </Text>
          {it.status_reason && <Text size="sm">Reason: {it.status_reason}</Text>}
          {it.linked_kind && <Text size="sm">Associated with {it.linked_kind} <span className="mono">{it.linked_id}</span></Text>}
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
      <Attachments spaceId={spaceId} list={it.attachments ?? []} />
      {Object.keys(it.trusted ?? {}).length > 0 && (
        <Stack gap={4}>
          <Text size="sm" fw={600}>Source metadata</Text>
          <Text size="xs" c="dimmed">Set by the authenticated source, not by the content.</Text>
          <Code block className="mono">{JSON.stringify(it.trusted, null, 2)}</Code>
        </Stack>
      )}
      {tasking && <TaskModal spaceId={spaceId} opened onClose={() => setTasking(false)} onCreated={() => void item.refetch()}
        seed={{ title: it.title, objective: it.content_type === 'text/markdown' ? it.body : undefined, fromInboxItem: it.id }} />}
      <StatusDialog kind={dialog} onClose={() => setDialog(null)} onSubmit={(extra) => void setStatus(dialog === 'defer' ? 'deferred' : 'rejected', extra)} />
    </Stack>
  )
}

function StatusDialog({ kind, onClose, onSubmit }: { kind: 'reject' | 'defer' | null; onClose: () => void; onSubmit: (v: { reason?: string; defer_until?: string }) => void }) {
  const [reason, setReason] = useState('')
  const [until, setUntil] = useState('')
  return (
    <Modal opened={kind !== null} onClose={onClose} title={kind === 'defer' ? 'Defer item' : 'Reject item'}>
      <Stack gap="sm">
        {kind === 'defer' && <TextInput type="datetime-local" label="Until" value={until} onChange={(e) => setUntil(e.target.value)} required />}
        <Textarea label="Reason" autosize minRows={2} value={reason} onChange={(e) => setReason(e.target.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={kind === 'reject' ? 'owlRed' : undefined} disabled={kind === 'defer' && !until}
            onClick={() => onSubmit({ reason: reason || undefined, defer_until: kind === 'defer' ? new Date(until).toISOString() : undefined })}>
            {kind === 'defer' ? 'Defer' : 'Reject'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
