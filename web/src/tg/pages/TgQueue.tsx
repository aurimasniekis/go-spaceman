import { ActionIcon, Button, Card, Checkbox, Group, Stack, Text, UnstyledButton } from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconGripVertical, IconPlus } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap, type Task } from '@/api/client'
import { keys, useMe, useQueue } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { LocalErrorBoundary } from '@/components/LocalErrorBoundary'
import { TaskPriority, TaskRowState } from '@/features/queue/badges'
import { dropOn, step } from '@/features/queue/order'
import { TaskModal } from '@/features/queue/TaskModal'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError } from '@/lib/notify'
import { dropBody, REORDER_ATTR, useTouchReorder } from './touchReorder'
import { TgTaskSheet } from './TgTaskSheet'

const FINISHED = ['done', 'cancelled']

/**
 * The queue in the Mini App (plan §4.6, Q10): the tasks in the order they run, reordered by dragging a row's grip with a
 * finger (or the mouse, or the arrow buttons), and "New task" for operators. The server enforces who may move or create
 * (the same rules as the web); the buttons only hide what the role cannot do. The queue's "next" view and policy are
 * web-only operations and are not shown here.
 */
export function TgQueue({ spaceId, open, onOpen }: { spaceId: string; open: string | null; onOpen: (id: string | null) => void }) {
  const qc = useQueryClient()
  const queue = useQueue(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const [creating, setCreating] = useState(false)
  const [showDone, setShowDone] = useState(false)
  const [dragged, setDragged] = useState<Task | null>(null)
  const all = queue.data?.tasks ?? []
  const tasks = showDone ? all : all.filter((t) => !FINISHED.includes(t.status))

  const move = async (id: string, body: { before_id?: string; after_id?: string }) => {
    try {
      unwrap(await api.POST('/spaces/{space_id}/tasks/{task_id}/move', { params: { path: { space_id: spaceId, task_id: id } }, body }))
      void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
    } catch (e) {
      notifyError(e, 'Could not move the task')
    }
  }
  const touch = useTouchReorder((from, over) => {
    const body = dropBody(all, from, over)
    if (body) void move(from, body)
  })

  return (
    <>
      <SectionHeader title="Queue" description="Tasks, in the order they run"
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New task</Button>} />
      <ErrorAlert error={queue.error} />
      <Group justify="space-between" mb={6} gap="xs">
        <Text size="xs" c="dimmed">{canWrite ? 'Hold the grip and drag to reorder.' : ''}</Text>
        <Checkbox size="xs" label="Show done and cancelled" checked={showDone} onChange={(e) => setShowDone(e.currentTarget.checked)} />
      </Group>
      <Stack gap={4} role="list" aria-label="Queue">
        {tasks.map((t) => (
          <Card key={t.id} p="xs" role="listitem" {...{ [REORDER_ATTR]: t.id }} draggable={canWrite}
            onDragStart={() => setDragged(t)} onDragEnd={() => setDragged(null)}
            onDragOver={(e) => { if (dragged && dropOn(dragged, t)) e.preventDefault() }}
            onDrop={(e) => { e.preventDefault(); const b = dragged && dropBody(all, dragged.id, t.id); if (b && dragged) void move(dragged.id, b) }}
            style={{
              marginLeft: Math.min(t.depth, 3) * 16, opacity: FINISHED.includes(t.status) ? 0.6 : 1,
              borderColor: touch.over === t.id ? 'var(--mantine-primary-color-filled)' : undefined,
              boxShadow: touch.dragging === t.id ? 'var(--mantine-shadow-md)' : undefined,
            }}>
            <Group gap="xs" wrap="nowrap">
              {canWrite && (
                <ActionIcon variant="subtle" color="gray" size={40} aria-label={`Drag ${t.title} to reorder`} {...touch.handle(t.id)}>
                  <IconGripVertical size={18} />
                </ActionIcon>
              )}
              <Stack gap={4} style={{ flex: 1, minWidth: 0 }}>
                <UnstyledButton ta="left" fz="sm" fw={t.status === 'running' ? 600 : 400} onClick={() => onOpen(t.id)} aria-label={`${t.position}. ${t.title}, ${t.status}`}>
                  <Text size="sm" fw="inherit" lineClamp={2}>{t.title}</Text>
                </UnstyledButton>
                <Group gap={4}><TaskRowState task={t} /><TaskPriority task={t} /></Group>
              </Stack>
              {canWrite && (
                <Stack gap={0}>
                  <ActionIcon aria-label={`Move ${t.title} up`} disabled={!step(all, t, -1)} onClick={() => { const b = step(all, t, -1); if (b) void move(t.id, b) }}><IconArrowUp size={16} /></ActionIcon>
                  <ActionIcon aria-label={`Move ${t.title} down`} disabled={!step(all, t, 1)} onClick={() => { const b = step(all, t, 1); if (b) void move(t.id, b) }}><IconArrowDown size={16} /></ActionIcon>
                </Stack>
              )}
            </Group>
          </Card>
        ))}
        {queue.isSuccess && tasks.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">The queue is empty.</Text>}
      </Stack>
      <LocalErrorBoundary title="The new-task form could not be shown">
        <TaskModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)} />
      </LocalErrorBoundary>
      <LocalErrorBoundary key={open ?? ''} title="This task could not be shown">
        <TgTaskSheet spaceId={spaceId} task={all.find((t) => t.id === open) ?? null} tasks={all} canWrite={canWrite} onClose={() => onOpen(null)} />
      </LocalErrorBoundary>
    </>
  )
}
