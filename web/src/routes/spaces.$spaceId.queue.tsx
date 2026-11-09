import { ActionIcon, Badge, Button, Card, Checkbox, Divider, Group, Modal, SegmentedControl, Stack, Text, Tooltip } from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconGripVertical, IconHandStop, IconPlus, IconSettings } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState, type KeyboardEvent } from 'react'
import { api, unwrap, type QueuePolicy, type Run, type Task } from '@/api/client'
import { keys, useMe, useNext, useQueue, useRuns } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { LocalErrorAlert, LocalErrorBoundary } from '@/components/LocalErrorBoundary'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { TaskPriority, TaskRowState } from '@/features/queue/badges'
import { dropOn, step } from '@/features/queue/order'
import { TaskDrawer } from '@/features/queue/TaskDrawer'
import { TaskModal } from '@/features/queue/TaskModal'
import { useAppForm } from '@/components/form/appForm'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/queue')({
  // ?task=<id> opens that task's drawer (links from human actions and the attention view, SM-HUI-006).
  validateSearch: (s: Record<string, unknown>): { task?: string } => (typeof s.task === 'string' ? { task: s.task } : {}),
  component: QueuePage,
  // A failure inside the queue stays in the queue: the space's header and navigation keep working.
  errorComponent: ({ error, reset }) => <LocalErrorAlert error={error} title="The queue could not be shown" reset={reset} />,
})

function QueuePage() {
  const { spaceId } = Route.useParams()
  const qc = useQueryClient()
  const queue = useQueue(spaceId)
  const runs = useRuns(spaceId)
  const next = useNext(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [creating, setCreating] = useState(false)
  const search = Route.useSearch()
  const navigate = Route.useNavigate()
  const [openState, setOpenState] = useState<string | null>(null)
  const open = openState ?? search.task ?? null
  const setOpen = (id: string | null) => {
    setOpenState(id)
    if (!id && search.task) void navigate({ search: {} })
  }
  const [dragging, setDragging] = useState<Task | null>(null)
  const [showDone, setShowDone] = useState(false)
  const [policyOpen, setPolicyOpen] = useState(false)
  const all = queue.data?.tasks ?? []
  const tasks = showDone ? all : all.filter((t) => !['done', 'cancelled'].includes(t.status))
  const selected = next.data?.selected?.id

  const move = async (t: Task, body: { before_id?: string; after_id?: string; to?: 'top' | 'bottom' }) => {
    try {
      unwrap(await api.POST('/spaces/{space_id}/tasks/{task_id}/move', { params: { path: { space_id: spaceId, task_id: t.id } }, body }))
      void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
    } catch (e) {
      notifyError(e, 'Could not move the task')
    }
  }
  // Keyboard sorting (SM-UI-006): Alt+↑/↓ moves the focused task among its siblings.
  const onKey = (e: KeyboardEvent, t: Task) => {
    if (!canWrite || !e.altKey || (e.key !== 'ArrowUp' && e.key !== 'ArrowDown')) {
      if (e.key === 'Enter') setOpen(t.id)
      return
    }
    e.preventDefault()
    const body = step(all, t, e.key === 'ArrowUp' ? -1 : 1)
    if (body) void move(t, body)
  }

  return (
    <>
      <SectionHeader title="Queue" description="The work queue, in the order it runs"
        actions={canWrite && (
          <>
            <Button variant="default" leftSection={<IconSettings size={14} />} onClick={() => setPolicyOpen(true)}>Policy</Button>
            <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New task</Button>
          </>
        )} />
      <ErrorAlert error={queue.error} />
      <Card mb="sm" p="sm">
        {/* Wraps on a phone, so the next task's title keeps a readable width. */}
        <Group gap="xs">
          <Text size="sm" fw={600}>Next:</Text>
          {next.data?.selected ? <Text size="sm" truncate style={{ minWidth: 'min(160px, 100%)' }}>{next.data.selected.title}</Text> : <Text size="sm" c="dimmed">nothing can start now</Text>}
          {(next.data?.skipped ?? []).length > 0 && (
            <Tooltip multiline w={420} label={(next.data?.skipped ?? []).slice(0, 8).map((s) => `${s.title}: ${s.reason}`).join('\n')} style={{ whiteSpace: 'pre-line' }}>
              <Text size="sm" c="dimmed" td="underline" style={{ textDecorationStyle: 'dotted', cursor: 'help', whiteSpace: 'nowrap' }}>
                · {next.data!.skipped!.length} {next.data!.skipped!.length === 1 ? 'task waits' : 'tasks wait'}
              </Text>
            </Tooltip>
          )}
          <Text size="xs" c="dimmed" ml="auto" style={{ whiteSpace: 'nowrap' }}>
            {(() => { const n = queue.data?.policy.max_active ?? 1; return `At most ${n} top-level ${n === 1 ? 'task' : 'tasks'} at a time` })()}
          </Text>
        </Group>
      </Card>
      <Group justify="space-between" mb={6}>
        <Text size="xs" c="dimmed">{canWrite ? 'Drag rows, or focus one and press Alt+↑/↓, to reorder. Enter opens a task.' : ''}</Text>
        <Checkbox size="xs" label="Show done and cancelled" checked={showDone} onChange={(e) => setShowDone(e.currentTarget.checked)} />
      </Group>
      <Stack gap={4} role="list" aria-label="Queue">
        {tasks.map((t) => (
          <Card key={t.id} p="xs" role="listitem" tabIndex={0} aria-label={`${t.position}. ${t.title}, ${t.status}`}
            draggable={canWrite} onDragStart={() => setDragging(t)} onDragEnd={() => setDragging(null)}
            onDragOver={(e) => { if (dragging && dropOn(dragging, t)) e.preventDefault() }}
            onDrop={(e) => { e.preventDefault(); const b = dragging && dropOn(dragging, t); if (b && dragging) void move(dragging, b) }}
            onKeyDown={(e) => onKey(e, t)} onClick={() => setOpen(t.id)}
            style={{ marginLeft: t.depth * 28, cursor: 'pointer', opacity: ['done', 'cancelled'].includes(t.status) ? 0.6 : 1,
              borderColor: t.id === selected ? 'var(--mantine-color-owlGreen-6)' : undefined }}>
            <Group gap="xs" wrap="nowrap">
              {canWrite && <IconGripVertical size={14} style={{ cursor: 'grab', flexShrink: 0 }} aria-hidden />}
              <Text size="xs" c="dimmed" w={24} ta="right">{t.position}</Text>
              <TaskRowState task={t} />
              <TaskPriority task={t} />
              <Text size="sm" fw={t.status === 'running' ? 600 : 400} style={{ flex: 1 }} truncate>{t.title}</Text>
              {t.manual && <Tooltip label="Placed by an operator: duplicate triggers never move it"><IconHandStop size={14} aria-label="Placed by an operator" /></Tooltip>}
              {t.status !== 'ready' && t.blockers.length > 0 && <Badge variant="light" color="owlRed">Blocked · {t.blockers.length}</Badge>}
              {t.assignee_run && <AssigneeRun spaceId={spaceId} runId={t.assignee_run} runs={runs.data} />}
              {canWrite && (
                <Group gap={0} onClick={(e) => e.stopPropagation()}>
                  <ActionIcon aria-label={`Move ${t.title} up`} disabled={!step(all, t, -1)} onClick={() => { const b = step(all, t, -1); if (b) void move(t, b) }}><IconArrowUp size={14} /></ActionIcon>
                  <ActionIcon aria-label={`Move ${t.title} down`} disabled={!step(all, t, 1)} onClick={() => { const b = step(all, t, 1); if (b) void move(t, b) }}><IconArrowDown size={14} /></ActionIcon>
                </Group>
              )}
            </Group>
          </Card>
        ))}
        {queue.isSuccess && tasks.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">The queue is empty. Tasks are created here or from inbox items.</Text>}
      </Stack>
      {/* One failing dialog or drawer must not take the queue list down with it. */}
      <LocalErrorBoundary title="The new-task form could not be shown">
        <TaskModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)} />
      </LocalErrorBoundary>
      <LocalErrorBoundary key={open ?? ''} title="This task could not be shown">
        <TaskDrawer spaceId={spaceId} task={all.find((t) => t.id === open) ?? null} tasks={all} canWrite={canWrite} onClose={() => setOpen(null)} />
      </LocalErrorBoundary>
      <LocalErrorBoundary title="The queue policy could not be shown">
        {queue.data && policyOpen && <PolicyModal spaceId={spaceId} policy={queue.data.policy} onClose={() => setPolicyOpen(false)} />}
      </LocalErrorBoundary>
    </>
  )
}

function PolicyModal({ spaceId, policy, onClose }: { spaceId: string; policy: QueuePolicy; onClose: () => void }) {
  const qc = useQueryClient()
  // The incident fields are optional in the API (an omitted one takes its default); the form always sends them.
  const form = useAppForm({
    defaultValues: {
      ...policy,
      incident_triggers: policy.incident_triggers ?? ['control', 'operator'],
      incident_untrusted: policy.incident_untrusted ?? 'propose',
      incident_workers: policy.incident_workers ?? 'finish',
      incident_capacity: policy.incident_capacity ?? 0,
      incident_resume: policy.incident_resume ?? 'manual',
    },
    onSubmit: async ({ value }) => {
      try {
        unwrap(await api.PUT('/spaces/{space_id}/queue/policy', { params: { path: { space_id: spaceId } }, body: value }))
        void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
        onClose()
      } catch (e) {
        notifyError(e)
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title="Queue policy" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <form.AppForm>
          <Stack gap="sm">
            <form.AppField name="max_active">{(f) => <f.NumberField label="Top-level tasks running at once" min={1} max={20} fallback={1} />}</form.AppField>
            <form.Field name="incident_insertion">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500} id="incident-insertion-label">New incident tasks go</Text>
                  <SegmentedControl aria-labelledby="incident-insertion-label" value={f.state.value} onChange={(v) => f.handleChange(v as QueuePolicy['incident_insertion'])}
                    data={[{ value: 'top', label: 'To the top' }, { value: 'priority', label: 'By priority' }]} />
                </Stack>
              )}
            </form.Field>
            <form.AppField name="incident_preempts">
              {(f) => <f.CheckboxField label="A new incident preempts running ordinary tasks (they return to ready; this is not a failure)" />}
            </form.AppField>
            <form.AppField name="escalation_reorders">
              {(f) => <f.CheckboxField label="Escalation may move a task to the top (otherwise it only raises priority)" />}
            </form.AppField>
            <Divider label="Incidents" labelPosition="left" />
            <form.AppField name="incident_triggers">
              {(f) => <f.TagsField label="Trusted incident triggers" clearable
                description="Who may raise incidents: operator, control, subscription:<id>, template:<id or slug>. Empty: nobody." />}
            </form.AppField>
            <form.Field name="incident_untrusted">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500} id="incident-untrusted-label">An incident from any other source</Text>
                  <SegmentedControl aria-labelledby="incident-untrusted-label" value={f.state.value} onChange={(v) => f.handleChange(v as 'propose' | 'refuse')}
                    data={[{ value: 'propose', label: 'Becomes a proposal' }, { value: 'refuse', label: 'Is refused' }]} />
                </Stack>
              )}
            </form.Field>
            <form.Field name="incident_workers">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500} id="incident-workers-label">Workers of a preempted task</Text>
                  <SegmentedControl aria-labelledby="incident-workers-label" value={f.state.value} onChange={(v) => f.handleChange(v as 'finish' | 'pause' | 'interrupt')}
                    data={[{ value: 'finish', label: 'Finish' }, { value: 'pause', label: 'Pause' }, { value: 'interrupt', label: 'Stop' }]} />
                  <Text size="xs" c="dimmed">Finish: they keep running and are told to wrap up. Pause: their turn is interrupted and they wait. Stop: they end, with a resumable session.</Text>
                </Stack>
              )}
            </form.Field>
            <form.AppField name="incident_capacity">
              {(f) => <f.NumberField label="Extra top-level slots for incidents" description="Incidents may run beside this many more tasks without preempting" min={0} max={20} fallback={0} />}
            </form.AppField>
            <form.Field name="incident_resume">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500} id="incident-resume-label">When the incident ends</Text>
                  <SegmentedControl aria-labelledby="incident-resume-label" value={f.state.value} onChange={(v) => f.handleChange(v as 'manual' | 'auto')}
                    data={[{ value: 'manual', label: 'Resume manually' }, { value: 'auto', label: 'Resume automatically' }]} />
                </Stack>
              )}
            </form.Field>
            <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><form.SubmitButton>Save</form.SubmitButton></Group>
          </Stack>
        </form.AppForm>
      </form>
    </Modal>
  )
}

/** The run working on a task: its role and turn, linking to its terminal (UX review: running tasks did not say who). */
function AssigneeRun({ spaceId, runId, runs }: { spaceId: string; runId: string; runs?: Run[] }) {
  const r = runs?.find((x) => x.id === runId)
  const live = r && !['exited', 'failed', 'lost'].includes(r.observed)
  return (
    <Tooltip label={r ? `${r.role} run ${runId}${live ? '' : ` (${r.observed})`}` : runId}>
      <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId }} size="xs" onClick={(e) => e.stopPropagation()} style={{ whiteSpace: 'nowrap' }}>
        {r ? `${r.role}${r.turn ? ` #${r.turn}` : ''}` : 'run'}{live ? '' : ' (ended)'} →
      </AnchorLink>
    </Tooltip>
  )
}
