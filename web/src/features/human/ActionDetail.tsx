import { Anchor, Badge, Button, Card, Divider, Group, Modal, Stack, Text, Title } from '@mantine/core'
import { IconHandGrab, IconUserMinus } from '@tabler/icons-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type HumanAction, type HumanNotification } from '@/api/client'
import { keys, useMe } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { AttachmentList } from '@/components/AttachmentList'
import { useAppForm } from '@/components/form/appForm'
import { MarkdownView } from '@/components/MarkdownView'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { markOwnChange } from '@/features/notifications/notify'
import { FollowUpBadges, HumanStatusBadge, isPending, notificationTarget, PriorityBadge, requesterLabel, routeReasonLabel, TypeBadge, type Resp } from './badges'
import { EnforcedApprovalCard, useActionApproval } from './EnforcedApproval'
import { CommentBox } from './CommentBox'
import { RespondForm } from './RespondForm'
import { DataView } from './SchemaForm'

/** One human action: the request, its response form or recorded response, assignment, comments and notifications. */
export function ActionDetail({ action: listed, spaceName, onChanged }: { action: HumanAction; spaceName?: string; onChanged?: (a: HumanAction) => void }) {
  const qc = useQueryClient()
  // Lists omit comments, notifications and the custom type spec: the drawer reads the full action.
  const full = useQuery({ queryKey: ['human-action', listed.space_id, listed.id, listed.version],
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/human-actions/{action_id}', { params: { path: { space_id: listed.space_id, action_id: listed.id } } })) })
  const action = full.data && full.data.version >= listed.version ? full.data : listed
  const enforced = useActionApproval(action)
  const me = useMe().data
  const role = roleIn(me, action.space_id)
  const canRespond = atLeast(role, 'operator')
  const [cancelling, setCancelling] = useState(false)
  const pending = isPending(action)
  const resp = (action.response ?? {}) as Resp
  const path = { space_id: action.space_id, action_id: action.id }

  const done = (a: HumanAction) => {
    void qc.invalidateQueries({ queryKey: keys.human(action.space_id) })
    void qc.invalidateQueries({ queryKey: keys.humanPending })
    void qc.invalidateQueries({ queryKey: ['human', 'search'] })
    onChanged?.(a)
  }
  const assign = async (assignee: string) => {
    markOwnChange(action.id)
    try {
      done(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/assign', { params: { path, header: { 'If-Match': ifMatch(action.version) } }, body: { assignee } })))
    } catch (e) {
      notifyError(e, 'Could not change the assignment')
    }
  }
  const cancelForm = useAppForm({
    defaultValues: { reason: '' },
    onSubmit: async ({ value }) => {
      try {
        done(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/cancel', {
          params: { path, header: { 'If-Match': ifMatch(action.version) } }, body: { reason: value.reason } })))
        setCancelling(false)
        cancelForm.reset()
        notifyOk('Withdrawn')
      } catch (e) {
        notifyError(e, 'Could not withdraw the action')
      }
    },
  })

  return (
    <Stack gap="md">
      <Stack gap={4}>
        <Group gap="xs"><TypeBadge type={action.type} slug={action.type_slug} /><HumanStatusBadge action={action} /><PriorityBadge priority={action.priority} />
          <FollowUpBadges action={action} />
          {action.blocks_task && <Badge variant="outline" color="owlYellow">blocks its task</Badge>}</Group>
        <Title order={3}>{action.title}</Title>
        <Text size="sm" c="dimmed">
          {action.requester_kind === 'agent' && action.requester_run
            ? <>{action.requester_label || action.requester_role || 'An agent'} · run{' '}
              <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId: action.space_id, runId: action.requester_run }} size="sm" className="mono">{action.requester_run}</AnchorLink></>
            : requesterLabel(action)}
          {' '}· {spaceName ? `${spaceName} · ` : ''}asked {ago(action.created_at)}
          {action.due_at ? ` · due ${datetime(action.due_at)}` : ''}{action.expires_at ? ` · expires ${datetime(action.expires_at)}` : ''}
        </Text>
        {(action.reminder_count > 0 || action.escalated_at || (pending && (action.next_reminder_at || action.escalate_at))) && (
          <Text size="sm" c="dimmed">
            {[action.reminder_count > 0 && `Reminded ${action.reminder_count}× (last ${ago(action.reminded_at!)})`,
              pending && action.next_reminder_at && `next reminder ${datetime(action.next_reminder_at)}`,
              action.escalated_at && `escalated ${ago(action.escalated_at)}${action.escalated_from && action.escalated_from !== action.priority ? ` from ${action.escalated_from}` : ''}`,
              pending && action.escalate_at && `escalates ${datetime(action.escalate_at)}`].filter(Boolean).join(' · ')}
          </Text>
        )}
        {action.task_id && <Text size="sm">Task: <AnchorLink to="/spaces/$spaceId/queue" params={{ spaceId: action.space_id }} search={{ task: action.task_id }} size="sm" className="mono">{action.task_id}</AnchorLink></Text>}
        {action.inbox_item_id && <Text size="sm">Inbox item: <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId: action.space_id, itemId: action.inbox_item_id }} size="sm" className="mono">{action.inbox_item_id}</AnchorLink></Text>}
        {action.follow_up_of && <Text size="sm">Follow-up of <AnchorLink to="/spaces/$spaceId/human" params={{ spaceId: action.space_id }} search={{ action: action.follow_up_of }} size="sm" className="mono">{action.follow_up_of}</AnchorLink> (the original is unchanged)</Text>}
      </Stack>

      {action.body && (
        <Card>
          {action.requester_kind === 'agent' && <Badge variant="outline" color="gray" mb="xs">written by an agent: untrusted content</Badge>}
          <MarkdownView source={action.body} spaceId={action.space_id} />
        </Card>
      )}
      {(action.attachments ?? []).length > 0 && <AttachmentList spaceId={action.space_id} list={action.attachments ?? []} />}

      {action.type === 'custom' && (
        <Card withBorder>
          <Text fw={600} mb={4}>Request</Text>
          <Text size="xs" c="dimmed" mb="xs">Type {action.type_slug} v{action.type_version}{action.requester_kind === 'agent' ? ' · written by an agent: untrusted content' : ''}</Text>
          <DataView data={action.request} schema={action.type_spec?.request_schema} ui={action.type_spec?.ui} />
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
          {action.type === 'custom' && resp.data !== undefined && <DataView data={resp.data} schema={action.type_spec?.response_schema} ui={action.type_spec?.ui} />}
          {resp.note && <Text size="sm" mt={4}>Note: {resp.note}</Text>}
          {action.status_reason && <Text size="sm" mt={4}>Reason: {action.status_reason}</Text>}
          {(action.response_attachments ?? []).length > 0 && <AttachmentList spaceId={action.space_id} list={action.response_attachments ?? []} title="Attached to the response" />}
          <Text size="xs" c="dimmed" mt="xs">Responses are final. To correct one, create a follow-up.</Text>
        </Card>
      )}
      {enforced.data && <EnforcedApprovalCard approval={enforced.data} />}
      {(action.notifications ?? []).map((n) => (
        <DeliveryCard key={n.id} action={action} n={n} canRoute={canRespond} onRouted={done} />
      ))}

      <Divider label="Comments (discussion, not a response)" labelPosition="left" />
      <Stack gap={6}>
        {(action.comments ?? []).map((c) => (
          <Card key={c.id} p="xs" withBorder={c.kind !== 'comment'}>
            <Group gap="xs">
              {c.kind === 'clarification' && <Badge size="xs" variant="light" color="owlBlue">clarification requested</Badge>}
              {c.kind === 'reply' && <Badge size="xs" variant="light" color="owlGreen">requester's reply</Badge>}
              <Text size="xs" c="dimmed">{c.author_label || c.author} · {ago(c.created_at)}</Text>
            </Group>
            <MarkdownView source={c.body} spaceId={action.space_id} />
            {(c.attachments ?? []).length > 0 && <AttachmentList spaceId={action.space_id} list={c.attachments ?? []} title="Attached" />}
          </Card>
        ))}
        {(action.comments ?? []).length === 0 && <Text size="sm" c="dimmed">No comments.</Text>}
        {canRespond && <CommentBox action={action} onDone={done} />}
      </Stack>
      {pending && canRespond && <Anchor size="sm" c="owlRed" component="button" onClick={() => setCancelling(true)} style={{ alignSelf: 'flex-start' }}>Withdraw this action…</Anchor>}
      <Modal opened={cancelling} onClose={() => setCancelling(false)} title="Withdraw the action">
        <form onSubmit={(e) => { e.preventDefault(); void cancelForm.handleSubmit() }}>
          <Stack gap="sm">
            <Text size="sm">The requester is notified, and a task it blocks is unblocked.</Text>
            <cancelForm.AppField name="reason">{(f) => <f.TextareaField label="Reason" autosize minRows={2} />}</cancelForm.AppField>
            <Group justify="flex-end"><cancelForm.AppForm><cancelForm.SubmitButton color="owlRed">Withdraw</cancelForm.SubmitButton></cancelForm.AppForm></Group>
          </Stack>
        </form>
      </Modal>
    </Stack>
  )
}

/** Where the requester's notification of the outcome went (SM-HUM-009): its target, provenance and state. */
function DeliveryCard({ action, n, canRoute, onRouted }: { action: HumanAction; n: HumanNotification; canRoute: boolean; onRouted: (a: HumanAction) => void }) {
  const undeliverable = n.target_kind === 'operators'
  const open = n.state !== 'acknowledged'
  const route = async (to: string) => {
    try {
      onRouted(unwrap(await api.POST('/spaces/{space_id}/human-actions/{action_id}/notifications/{notification_id}/route', {
        params: { path: { space_id: action.space_id, action_id: action.id, notification_id: n.id } }, body: { to } })))
      notifyOk('Sent to control')
    } catch (e) {
      notifyError(e, 'Could not route the response')
    }
  }
  return (
    <Card withBorder p="xs">
      <Group justify="space-between" wrap="nowrap" align="flex-start">
        <Stack gap={2}>
          <Group gap="xs">
            <Text size="sm" fw={600}>Requester notified ({n.kind})</Text>
            {undeliverable ? <Badge color="owlRed" variant="light">undeliverable</Badge>
              : <Badge variant="light" color={n.state === 'acknowledged' ? 'owlGreen' : 'gray'}>{n.state}</Badge>}
            {n.route_reason && !undeliverable && <Badge variant="outline" color="owlPurple">rerouted</Badge>}
            {!n.wake && <Badge variant="outline" color="gray">no terminal notice</Badge>}
          </Group>
          <Text size="xs" c="dimmed">To {notificationTarget(n)}{n.target_kind === 'instance' && n.hold_until ? `, until ${datetime(n.hold_until)}` : ''}</Text>
          {n.route_reason && (
            <Text size="xs" c="dimmed">
              Asked by run {n.original_run}{n.routed_from && n.routed_from !== `run:${n.original_run}` ? `; last routed from ${n.routed_from}` : ''}. Routed because{' '}
              {routeReasonLabel[n.route_reason] ?? n.route_reason}{n.routed_at ? ` (${ago(n.routed_at)})` : ''}.
            </Text>
          )}
        </Stack>
        {canRoute && open && n.target_kind !== 'control' && undeliverable && (
          <Button size="xs" variant="default" onClick={() => void route('control')}>Send to control</Button>
        )}
      </Group>
    </Card>
  )
}
