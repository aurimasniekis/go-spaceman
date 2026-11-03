import { ActionIcon, Alert, Badge, Button, Divider, Drawer, Group, Menu, Stack, Table, TagsInput, Text, Title, Tooltip } from '@mantine/core'
import { IconArrowBigUpLines, IconChevronDown, IconPlayerPlay, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { api, ifMatch, unwrap, type Task } from '@/api/client'
import { keys } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { AttachmentList } from '@/components/AttachmentList'
import { useAppForm } from '@/components/form/appForm'
import { MarkdownView } from '@/components/MarkdownView'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { TaskPriority, TaskStatus, transitions } from './badges'
import { ReassignForm } from './ReassignForm'

const actionLabel: Record<string, string> = { finish: 'let finish', pause: 'paused', interrupt: 'stopped', preempted: 'preempted' }

/** A task's details and the operator's controls: state, dependencies, blockers, escalation. */
export function TaskDrawer({ spaceId, task, tasks, canWrite, onClose }: { spaceId: string; task: Task | null; tasks: Task[]; canWrite: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  // The reason goes with whichever state change or escalation comes next; the block note is its own small form.
  const reasonForm = useAppForm({ defaultValues: { reason: '' } })
  const blockForm = useAppForm({
    defaultValues: { note: '' },
    onSubmit: async ({ value }) => {
      if (!task) return
      await call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/blockers', {
        params: { path: { space_id: spaceId, task_id: task.id } }, body: { kind: 'external', note: value.note } }).then(unwrap).then(() => blockForm.reset()), 'Blocked')
    },
  })
  const reason = () => reasonForm.state.values.reason
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
  const call = async (fn: () => Promise<unknown>, ok: string) => {
    try {
      await fn()
      refresh()
      notifyOk(ok)
    } catch (e) {
      notifyError(e)
    }
  }
  if (!task) return <Drawer opened={false} onClose={onClose} />
  const t = task
  const path = { space_id: spaceId, task_id: t.id }
  const titleOf = (id: string) => tasks.find((x) => x.id === id)?.title ?? id
  const decide = (decision: 'confirm' | 'dismiss') => call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/incident', {
    params: { path, header: { 'If-Match': ifMatch(t.version) } }, body: { decision } }).then(unwrap), decision === 'confirm' ? 'Confirmed as an incident' : 'Proposal dismissed')
  const resume = async () => {
    try {
      const r = unwrap(await api.POST('/spaces/{space_id}/tasks/{task_id}/resume', { params: { path } }))
      refresh()
      notifyOk(`Resume: ${r.outcome}`)
    } catch (e) {
      notifyError(e, 'Could not resume the task')
    }
  }
  return (
    <Drawer opened onClose={onClose} position="right" size="lg" title={<Group gap="xs"><TaskStatus status={t.status} /><TaskPriority task={t} /></Group>}>
      <Stack gap="md">
        <Stack gap={2}>
          <Title order={4}>{t.title}</Title>
          <Text size="xs" c="dimmed">#{t.position} in the queue{t.manual ? ' · placed by an operator' : ''}{t.preemptions > 0 ? ` · preempted ${t.preemptions}×` : ''}</Text>
          {t.origin_kind === 'inbox_item' && (
            <Text size="xs">From inbox item <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId: t.origin_id! }} size="xs">{t.origin_id}</AnchorLink></Text>
          )}
          {t.status_reason && <Text size="sm">Reason: {t.status_reason}</Text>}
        </Stack>
        {t.incident_proposed_by && (
          <Alert color="owlRed" variant="light" title="Incident proposal">
            <Stack gap="xs">
              <Text size="sm">
                <span className="mono">{t.incident_proposed_by}</span> asked for this to be an incident, but the queue policy does not trust that source.
                It is an ordinary task until an operator confirms it; confirming applies the incident insertion and preemption now.
              </Text>
              {canWrite && (
                <Group gap="xs">
                  <Button color="owlRed" size="xs" onClick={() => void decide('confirm')}>Confirm incident</Button>
                  <Button variant="default" size="xs" onClick={() => void decide('dismiss')}>Dismiss</Button>
                </Group>
              )}
            </Stack>
          </Alert>
        )}
        {t.kind === 'incident' && t.incident_trigger && <Text size="xs" c="dimmed">Incident raised by <span className="mono">{t.incident_trigger}</span></Text>}
        {t.preempted_by && (
          <Alert color="owlYellow" variant="light" title="Preempted by an incident">
            <Stack gap="xs">
              <Text size="sm">Incident <span className="mono">{titleOf(t.preempted_by)}</span> returned this task to ready. This is not a failure. When it resumes, its live workers continue;
                if none are left, control launches a new one.</Text>
              {canWrite && <Group><Button size="xs" variant="default" leftSection={<IconPlayerPlay size={14} />} onClick={() => void resume()}>Resume</Button></Group>}
            </Stack>
          </Alert>
        )}
        {(t.preemption_log ?? []).length > 0 && (
          <Stack gap={2}>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Preemptions</Text>
            <Table aria-label="Preemption log" fz="xs" verticalSpacing={2}>
              <Table.Thead><Table.Tr><Table.Th>When</Table.Th><Table.Th>Incident</Table.Th><Table.Th>What</Table.Th><Table.Th>Outcome</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {t.preemption_log!.map((p) => (
                  <Table.Tr key={p.id}>
                    <Table.Td>{datetime(p.created_at)}</Table.Td>
                    <Table.Td>{titleOf(p.incident_id)}</Table.Td>
                    <Table.Td>{p.run_id ? <>worker <span className="mono">{p.run_id.slice(-8)}</span>: {actionLabel[p.action]}</> : 'task returned to ready'}</Table.Td>
                    <Table.Td>{p.resumed_at ? p.outcome : 'open'}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Stack>
        )}
        {(t.waiting ?? []).length > 0 && (
          <Stack gap={2}>
            <Text size="xs" fw={600} c="owlYellow">Not eligible now</Text>
            {t.waiting!.map((w) => <Text key={w} size="sm">• {w}</Text>)}
          </Stack>
        )}
        {canWrite && (
          <Group gap="xs">
            <Menu position="bottom-start">
              <Menu.Target><Button rightSection={<IconChevronDown size={12} />}>Change state</Button></Menu.Target>
              <Menu.Dropdown>
                {[...transitions[t.status], ...(t.status === 'ready' && t.preempted_by ? (['review'] as const) : [])].map((s) => (
                  <Menu.Item key={s} onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/status', {
                    params: { path, header: { 'If-Match': ifMatch(t.version) } }, body: { status: s, reason: reason() || undefined },
                  }).then(unwrap), `Now ${s}`)}>
                    {s === 'ready' && t.status === 'running' ? 'ready (preempt: not a failure)' : s}
                  </Menu.Item>
                ))}
              </Menu.Dropdown>
            </Menu>
            <Tooltip label="Raise to urgent (moves it to the top only if the queue policy allows)">
              <Button variant="default" leftSection={<IconArrowBigUpLines size={14} />}
                onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/escalate', { params: { path }, body: { reason: reason() || 'escalated by an operator' } }).then(unwrap), 'Escalated')}>
                Escalate
              </Button>
            </Tooltip>
          </Group>
        )}
        {canWrite && (
          <reasonForm.AppField name="reason">{(f) => <f.TextField label="Reason (for the next state change or escalation)" />}</reasonForm.AppField>
        )}
        {canWrite ? (
          <ReassignForm key={`${t.id}:${t.version}`} spaceId={spaceId} task={t} />
        ) : (
          <Text size="sm">Assignee: {t.assignee_role || 'any role'}{t.assignee_run ? <> · run <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: t.assignee_run }} size="sm" className="mono">{t.assignee_run}</AnchorLink></> : ''}</Text>
        )}
        <Divider />
        {t.objective && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Objective</Text><MarkdownView source={t.objective} spaceId={spaceId} /></Stack>}
        {t.acceptance && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Acceptance criteria</Text><MarkdownView source={t.acceptance} spaceId={spaceId} /></Stack>}
        {t.result && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Result</Text><MarkdownView source={t.result} spaceId={spaceId} /></Stack>}
        <AttachmentList spaceId={spaceId} list={t.attachments ?? []} cols={{ base: 2, sm: 3 }} />
        <Stack gap={4}>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Depends on</Text>
          {canWrite ? (
            <TagsInput aria-label="Depends on" value={t.depends_on.map(titleOf)} data={tasks.filter((x) => x.id !== t.id).map((x) => x.title)}
              onChange={(titles) => void call(() => api.PUT('/spaces/{space_id}/tasks/{task_id}/dependencies', {
                params: { path }, body: { depends_on: titles.map((x) => tasks.find((y) => y.title === x)?.id).filter((x): x is string => !!x) },
              }).then(unwrap), 'Dependencies saved')} />
          ) : (
            <Text size="sm">{t.depends_on.map(titleOf).join(', ') || 'none'}</Text>
          )}
        </Stack>
        <Stack gap={4}>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Blockers</Text>
          {t.blockers.map((b) => (
            <Group key={b.id} gap="xs" wrap="nowrap">
              <Badge variant="outline" color="owlYellow">{b.kind === 'human_action' ? 'human action' : 'external'}</Badge>
              <Text size="sm" style={{ flex: 1 }}>
                {b.kind === 'human_action' && b.ref_id
                  ? <AnchorLink to="/spaces/$spaceId/human" params={{ spaceId }} search={{ action: b.ref_id }} size="sm" className="mono">{b.ref_id}</AnchorLink>
                  : b.ref_id ? `${b.ref_id} ` : ''}{' '}{b.note}
              </Text>
              {canWrite && <ActionIcon aria-label="Remove blocker" onClick={() => void call(() => api.DELETE('/spaces/{space_id}/tasks/{task_id}/blockers/{blocker_id}', {
                params: { path: { ...path, blocker_id: b.id } } }).then(unwrap), 'Blocker removed')}><IconX size={14} /></ActionIcon>}
            </Group>
          ))}
          {t.blockers.length === 0 && <Text size="sm" c="dimmed">None</Text>}
          {canWrite && (
            <form onSubmit={(e) => { e.preventDefault(); void blockForm.handleSubmit() }}>
              <blockForm.AppForm>
                <Group gap="xs" wrap="nowrap" align="flex-start">
                  <blockForm.AppField name="note">
                    {(f) => <f.TextareaField aria-label="Blocker note" placeholder="What is it waiting for?" autosize minRows={1} style={{ flex: 1 }} />}
                  </blockForm.AppField>
                  <blockForm.Subscribe selector={(st) => st.values.note}>
                    {(note) => <blockForm.SubmitButton variant="default" disabled={!note.trim()}>Block</blockForm.SubmitButton>}
                  </blockForm.Subscribe>
                </Group>
              </blockForm.AppForm>
            </form>
          )}
        </Stack>
        <Text size="xs" c="dimmed">Created {datetime(t.created_at)}{t.started_at ? ` · started ${datetime(t.started_at)}` : ''}{t.finished_at ? ` · finished ${datetime(t.finished_at)}` : ''}</Text>
        {t.exclusions.length > 0 && <Text size="xs">Exclusive resources: <span className="mono">{t.exclusions.join(', ')}</span></Text>}
        {(t.assignee_run || t.workspace) && (
          <Text size="xs">
            {t.assignee_run && <>Worker <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: t.assignee_run }} size="xs" className="mono">{t.assignee_run}</AnchorLink></>}
            {t.workspace && <> in <AnchorLink to="/spaces/$spaceId/repositories" params={{ spaceId }} size="xs" className="mono">{t.workspace}</AnchorLink></>}
          </Text>
        )}
      </Stack>
    </Drawer>
  )
}
