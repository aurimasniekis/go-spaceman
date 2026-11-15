import { Alert, Button, Drawer, Group, Menu, Stack, Text, Title } from '@mantine/core'
import { IconChevronDown } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { api, ifMatch, unwrap, type Task } from '@/api/client'
import { keys } from '@/api/queries'
import { AttachmentList } from '@/components/AttachmentList'
import { MarkdownView } from '@/components/MarkdownView'
import { AnchorLink } from '@/components/AnchorLink'
import { TaskPriority, TaskStatus, transitions } from '@/features/queue/badges'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

/**
 * A task in a bottom sheet (Mini App): what it is, what it waits on, and the operator's state change and incident
 * decision, the task operations a Telegram link has. Blockers, dependencies, escalation and reassignment stay in the web.
 */
export function TgTaskSheet({ spaceId, task, tasks, canWrite, onClose }: { spaceId: string; task: Task | null; tasks: Task[]; canWrite: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  if (!task) return <Drawer opened={false} onClose={onClose} />
  const t = task
  const path = { space_id: spaceId, task_id: t.id }
  const titleOf = (id: string) => tasks.find((x) => x.id === id)?.title ?? id
  const call = async (fn: () => Promise<unknown>, ok: string) => {
    try {
      await fn()
      void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
      notifyOk(ok)
    } catch (e) {
      notifyError(e)
    }
  }
  const decide = (decision: 'confirm' | 'dismiss') => call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/incident', {
    params: { path, header: { 'If-Match': ifMatch(t.version) } }, body: { decision } }).then(unwrap), decision === 'confirm' ? 'Confirmed as an incident' : 'Proposal dismissed')
  const next = [...transitions[t.status], ...(t.status === 'ready' && t.preempted_by ? (['review'] as const) : [])]
  return (
    <Drawer opened onClose={onClose} position="bottom" size="85%" title={<Group gap="xs"><TaskStatus status={t.status} /><TaskPriority task={t} /></Group>}>
      <Stack gap="md">
        <Stack gap={2}>
          <Title order={4}>{t.title}</Title>
          <Text size="xs" c="dimmed">#{t.position} in the queue{t.manual ? ' · placed by an operator' : ''}</Text>
          {t.status_reason && <Text size="sm">Reason: {t.status_reason}</Text>}
        </Stack>
        {t.incident_proposed_by && (
          <Alert color="owlRed" variant="light" title="Incident proposal">
            <Stack gap="xs">
              <Text size="sm">An untrusted source asked for this to be an incident. It stays an ordinary task until an operator confirms it.</Text>
              {canWrite && <Group gap="xs"><Button color="owlRed" size="xs" onClick={() => void decide('confirm')}>Confirm incident</Button><Button variant="default" size="xs" onClick={() => void decide('dismiss')}>Dismiss</Button></Group>}
            </Stack>
          </Alert>
        )}
        {t.preempted_by && <Alert color="owlYellow" variant="light" title="Preempted by an incident">Incident {titleOf(t.preempted_by)} returned this task to ready. This is not a failure.</Alert>}
        {(t.waiting ?? []).length > 0 && (
          <Stack gap={2}><Text size="xs" fw={600} c="owlYellow">Not eligible now</Text>{t.waiting!.map((w) => <Text key={w} size="sm">• {w}</Text>)}</Stack>
        )}
        {canWrite && next.length > 0 && (
          <Menu position="bottom-start">
            <Menu.Target><Button rightSection={<IconChevronDown size={12} />} w="fit-content">Change state</Button></Menu.Target>
            <Menu.Dropdown>
              {next.map((s) => (
                <Menu.Item key={s} onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/status', {
                  params: { path, header: { 'If-Match': ifMatch(t.version) } }, body: { status: s },
                }).then(unwrap), `Now ${s}`)}>
                  {s === 'ready' && t.status === 'running' ? 'ready (preempt: not a failure)' : s}
                </Menu.Item>
              ))}
            </Menu.Dropdown>
          </Menu>
        )}
        {t.objective && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Objective</Text><MarkdownView source={t.objective} spaceId={spaceId} /></Stack>}
        {t.acceptance && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Acceptance criteria</Text><MarkdownView source={t.acceptance} spaceId={spaceId} /></Stack>}
        {t.result && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Result</Text><MarkdownView source={t.result} spaceId={spaceId} /></Stack>}
        <AttachmentList spaceId={spaceId} list={t.attachments ?? []} cols={{ base: 2, sm: 3 }} />
        {t.depends_on.length > 0 && <Text size="sm">Depends on: {t.depends_on.map(titleOf).join(', ')}</Text>}
        {t.blockers.length > 0 && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Blockers</Text>{t.blockers.map((b) => <Text key={b.id} size="sm">• {b.note || b.ref_id}</Text>)}</Stack>}
        <Text size="xs" c="dimmed">Created {datetime(t.created_at)}{t.started_at ? ` · started ${datetime(t.started_at)}` : ''}{t.finished_at ? ` · finished ${datetime(t.finished_at)}` : ''}</Text>
        {t.assignee_run && <Text size="xs">Worker <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: t.assignee_run }} size="xs" className="mono">{t.assignee_run}</AnchorLink></Text>}
      </Stack>
    </Drawer>
  )
}
