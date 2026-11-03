import { ActionIcon, Badge, Button, Divider, Drawer, Group, Menu, Stack, TagsInput, Text, Textarea, TextInput, Title, Tooltip } from '@mantine/core'
import { IconArrowBigUpLines, IconChevronDown, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Task } from '@/api/client'
import { keys } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { MarkdownView } from '@/components/MarkdownView'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { TaskPriority, TaskStatus, transitions } from './badges'

/** A task's details and the operator's controls: state, dependencies, blockers, escalation. */
export function TaskDrawer({ spaceId, task, tasks, canWrite, onClose }: { spaceId: string; task: Task | null; tasks: Task[]; canWrite: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [blockNote, setBlockNote] = useState('')
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
                {transitions[t.status].map((s) => (
                  <Menu.Item key={s} onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/status', {
                    params: { path, header: { 'If-Match': ifMatch(t.version) } }, body: { status: s, reason: reason || undefined },
                  }).then(unwrap), `Now ${s}`)}>
                    {s === 'ready' && t.status === 'running' ? 'ready (preempt: not a failure)' : s}
                  </Menu.Item>
                ))}
              </Menu.Dropdown>
            </Menu>
            <Tooltip label="Raise to urgent (moves it to the top only if the queue policy allows)">
              <Button variant="default" leftSection={<IconArrowBigUpLines size={14} />}
                onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/escalate', { params: { path }, body: { reason: reason || 'escalated by an operator' } }).then(unwrap), 'Escalated')}>
                Escalate
              </Button>
            </Tooltip>
          </Group>
        )}
        {canWrite && <TextInput label="Reason (for the next state change or escalation)" value={reason} onChange={(e) => setReason(e.target.value)} />}
        <Divider />
        {t.objective && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Objective</Text><MarkdownView source={t.objective} spaceId={spaceId} /></Stack>}
        {t.acceptance && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Acceptance criteria</Text><MarkdownView source={t.acceptance} spaceId={spaceId} /></Stack>}
        {t.result && <Stack gap={2}><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Result</Text><MarkdownView source={t.result} spaceId={spaceId} /></Stack>}
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
              <Text size="sm" style={{ flex: 1 }}>{b.ref_id ? `${b.ref_id} ` : ''}{b.note}</Text>
              {canWrite && <ActionIcon aria-label="Remove blocker" onClick={() => void call(() => api.DELETE('/spaces/{space_id}/tasks/{task_id}/blockers/{blocker_id}', {
                params: { path: { ...path, blocker_id: b.id } } }).then(unwrap), 'Blocker removed')}><IconX size={14} /></ActionIcon>}
            </Group>
          ))}
          {t.blockers.length === 0 && <Text size="sm" c="dimmed">None</Text>}
          {canWrite && (
            <Group gap="xs" wrap="nowrap">
              <Textarea placeholder="What is it waiting for?" autosize minRows={1} style={{ flex: 1 }} value={blockNote} onChange={(e) => setBlockNote(e.target.value)} />
              <Button variant="default" disabled={!blockNote.trim()} onClick={() => void call(() => api.POST('/spaces/{space_id}/tasks/{task_id}/blockers', {
                params: { path }, body: { kind: 'external', note: blockNote } }).then(unwrap).then(() => setBlockNote('')), 'Blocked')}>Block</Button>
            </Group>
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
