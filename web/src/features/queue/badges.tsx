import { Badge, Tooltip } from '@mantine/core'
import type { Task } from '@/api/client'

export const statusColor: Record<Task['status'], string> = {
  backlog: 'gray', ready: 'owlBlue', running: 'owlGreen', blocked: 'owlYellow', review: 'owlPurple', done: 'gray', failed: 'owlRed', cancelled: 'gray',
}
const priorityColor: Record<Task['priority'], string> = { low: 'gray', normal: 'owlBlue', high: 'owlYellow', urgent: 'owlRed' }

const STATUS_LABEL: Record<Task['status'], string> = { backlog: 'Backlog', ready: 'Ready', running: 'Running', review: 'Review', blocked: 'Blocked', done: 'Done', failed: 'Failed', cancelled: 'Cancelled' }

export function TaskStatus({ status }: { status: Task['status'] }) {
  return <Badge color={statusColor[status]} variant={status === 'running' ? 'filled' : 'light'}>{STATUS_LABEL[status] ?? status}</Badge>
}

/**
 * One state per row (UX review: a row read READY and WAITING at once). A ready task that waits on dependencies shows
 * "Waiting" with what it waits on; one with open blockers shows "Blocked"; otherwise the task's own state.
 */
export function TaskRowState({ task }: { task: Task }) {
  const waiting = task.waiting ?? []
  if (task.status === 'ready' && task.blockers.length > 0) {
    return <Tooltip label={`${task.blockers.length} open blocker${task.blockers.length > 1 ? 's' : ''}`}><Badge color="owlRed" variant="light">Blocked · {task.blockers.length}</Badge></Tooltip>
  }
  if (task.status === 'ready' && waiting.length > 0) {
    return <Tooltip label={waiting.join('; ')} multiline w={320}><Badge color="owlYellow" variant="light">Waiting</Badge></Tooltip>
  }
  return <TaskStatus status={task.status} />
}

export function TaskPriority({ task }: { task: Task }) {
  return (
    <>
      {task.kind === 'incident' && <Badge color="owlRed" variant="filled">incident</Badge>}
      {task.incident_proposed_by && <Badge color="owlRed" variant="outline" title={`Proposed by ${task.incident_proposed_by}; an operator confirms or dismisses it`}>incident proposed</Badge>}
      {task.preempted_by && <Badge color="owlYellow" variant="light" title="An incident preempted this task (not a failure); it waits to resume">preempted</Badge>}
      {task.priority !== 'normal' && <Badge color={priorityColor[task.priority]} variant="outline">{task.priority}</Badge>}
    </>
  )
}

/** Allowed state changes, mirroring the server (internal/queue transitions). */
export const transitions: Record<Task['status'], Task['status'][]> = {
  backlog: ['ready', 'cancelled'],
  ready: ['running', 'backlog', 'blocked', 'cancelled'],
  running: ['review', 'done', 'failed', 'blocked', 'ready', 'cancelled'],
  blocked: ['ready', 'running', 'cancelled', 'failed'],
  review: ['done', 'running', 'ready', 'failed', 'cancelled'],
  done: ['ready'],
  failed: ['ready', 'cancelled'],
  cancelled: ['backlog', 'ready'],
}
