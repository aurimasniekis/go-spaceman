import { Anchor, Badge, Button, Card, Divider, Group, Modal, Stack, Text, Textarea, Title } from '@mantine/core'
import { IconHandGrab, IconUserMinus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type HumanAction } from '@/api/client'
import { keys, useMe } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { MarkdownView } from '@/components/MarkdownView'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { HumanStatusBadge, isPending, PriorityBadge, requesterLabel, TypeBadge, type Resp } from './badges'
import { RespondForm } from './RespondForm'

/** One human action: the request, its response form or recorded response, assignment, comments and notifications. */
export function ActionDetail({ action, spaceName, onChanged }: { action: HumanAction; spaceName?: string; onChanged?: (a: HumanAction) => void }) {
  const qc = useQueryClient()
  const me = useMe().data
  const role = roleIn(me, action.space_id)
  const canRespond = atLeast(role, 'operator')
  const [comment, setComment] = useState('')
  const [cancelling, setCancelling] = useState(false)
  const [reason, setReason] = useState('')
  const pending = isPending(action)
  const resp = (action.response ?? {}) as Resp
  const path = { space_id: action.space_id, action_id: action.id }

  const done = (a: HumanAction) => {
    void qc.invalidateQueries({ queryKey: keys.human(action.space_id) })
    void qc.invalidateQueries({ queryKey: keys.humanPending })
    onChanged?.(a)
  }
  const assign = async (assignee: string) => {
    try {
      done(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/assign', { params: { path, header: { 'If-Match': ifMatch(action.version) } }, body: { assignee } })))
    } catch (e) {
      notifyError(e, 'Could not change the assignment')
    }
  }
  const addComment = async () => {
    try {
      done(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/comments', { params: { path }, body: { body: comment } })))
      setComment('')
    } catch (e) {
      notifyError(e, 'Could not add the comment')
    }
  }
  const cancel = async () => {
    try {
      done(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/cancel', { params: { path, header: { 'If-Match': ifMatch(action.version) } }, body: { reason } })))
      setCancelling(false)
      notifyOk('Withdrawn')
    } catch (e) {
      notifyError(e, 'Could not withdraw the action')
    }
  }

  return (
    <Stack gap="md">
      <Stack gap={4}>
        <Group gap="xs"><TypeBadge type={action.type} /><HumanStatusBadge action={action} /><PriorityBadge priority={action.priority} />
          {action.blocks_task && <Badge variant="outline" color="owlYellow">blocks its task</Badge>}</Group>
        <Title order={3}>{action.title}</Title>
        <Text size="sm" c="dimmed">
          {requesterLabel(action)} · {spaceName ? `${spaceName} · ` : ''}asked {ago(action.created_at)}
          {action.due_at ? ` · due ${datetime(action.due_at)}` : ''}{action.expires_at ? ` · expires ${datetime(action.expires_at)}` : ''}
        </Text>
        {action.task_id && <Text size="sm">Task: <AnchorLink to="/spaces/$spaceId/queue" params={{ spaceId: action.space_id }} size="sm">{action.task_id}</AnchorLink></Text>}
        {action.follow_up_of && <Text size="sm">Follow-up of <span className="mono">{action.follow_up_of}</span> (the original is unchanged)</Text>}
      </Stack>

      {action.body && (
        <Card>
          {action.requester_kind === 'agent' && <Badge variant="outline" color="gray" mb="xs">written by an agent: untrusted content</Badge>}
          <MarkdownView source={action.body} spaceId={action.space_id} />
        </Card>
      )}

      {pending && canRespond && (
        <Card withBorder>
          <Group justify="space-between" mb="sm">
            <Text fw={600}>Respond</Text>
            {action.assignee ? (
              <Group gap="xs">
                <Text size="sm" c="dimmed">Assigned to {action.assignee === me?.user?.id ? 'you' : <span className="mono">{action.assignee}</span>}</Text>
                <Button size="xs" variant="subtle" leftSection={<IconUserMinus size={14} />} onClick={() => void assign('')}>Unassign</Button>
              </Group>
            ) : me?.user && <Button size="xs" variant="default" leftSection={<IconHandGrab size={14} />} onClick={() => void assign(me.user!.id)}>Claim</Button>}
          </Group>
          <RespondForm action={action} canDecide={canRespond} onDone={done} />
        </Card>
      )}
      {!pending && (
        <Card withBorder>
          <Text fw={600} mb={4}>{action.status === 'resolved' ? `Response: ${action.outcome}` : action.status === 'cancelled' ? 'Withdrawn' : 'Expired without a response'}</Text>
          <Text size="sm" c="dimmed">{action.responded_by && `by ${action.responded_by} `}{action.responded_at && datetime(action.responded_at)}</Text>
          {resp.answer && <MarkdownView source={resp.answer} spaceId={action.space_id} />}
          {resp.note && <Text size="sm" mt={4}>Note: {resp.note}</Text>}
          {action.status_reason && <Text size="sm" mt={4}>Reason: {action.status_reason}</Text>}
          <Text size="xs" c="dimmed" mt="xs">Responses are final. To correct one, create a follow-up.</Text>
        </Card>
      )}
      {(action.notifications ?? []).length > 0 && (
        <Text size="xs" c="dimmed">Requester notified: {action.notifications!.map((n) => `${n.kind} (${n.state})`).join(', ')}</Text>
      )}

      <Divider label="Comments (discussion, not a response)" labelPosition="left" />
      <Stack gap={6}>
        {(action.comments ?? []).map((c) => (
          <Card key={c.id} p="xs">
            <Text size="xs" c="dimmed">{c.author_label || c.author} · {ago(c.created_at)}</Text>
            <MarkdownView source={c.body} spaceId={action.space_id} />
          </Card>
        ))}
        {(action.comments ?? []).length === 0 && <Text size="sm" c="dimmed">No comments.</Text>}
        {canRespond && (
          <Group align="flex-end" wrap="nowrap">
            <Textarea aria-label="Comment" placeholder="Add a comment" autosize minRows={1} maxRows={6} style={{ flex: 1 }} value={comment} onChange={(e) => setComment(e.target.value)} />
            <Button variant="default" disabled={!comment.trim()} onClick={() => void addComment()}>Comment</Button>
          </Group>
        )}
      </Stack>
      {pending && canRespond && <Anchor size="sm" c="owlRed" component="button" onClick={() => setCancelling(true)} style={{ alignSelf: 'flex-start' }}>Withdraw this action…</Anchor>}
      <Modal opened={cancelling} onClose={() => setCancelling(false)} title="Withdraw the action">
        <Stack gap="sm">
          <Text size="sm">The requester is notified, and a task it blocks is unblocked.</Text>
          <Textarea label="Reason" value={reason} onChange={(e) => setReason(e.target.value)} />
          <Group justify="flex-end"><Button color="owlRed" onClick={() => void cancel()}>Withdraw</Button></Group>
        </Stack>
      </Modal>
    </Stack>
  )
}
