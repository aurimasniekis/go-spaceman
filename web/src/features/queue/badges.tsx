import { Badge } from '@mantine/core'
import type { Task } from '@/api/client'

export const statusColor: Record<Task['status'], string> = {
  backlog: 'gray', ready: 'owlBlue', running: 'owlGreen', blocked: 'owlYellow', review: 'owlPurple', done: 'gray', failed: 'owlRed', cancelled: 'gray',
}
const priorityColor: Record<Task['priority'], string> = { low: 'gray', normal: 'owlBlue', high: 'owlYellow', urgent: 'owlRed' }

export function TaskStatus({ status }: { status: Task['status'] }) {
  return <Badge color={statusColor[status]} variant={status === 'running' ? 'filled' : 'light'}>{status}</Badge>
}

export function TaskPriority({ task }: { task: Task }) {
  return (
    <>
      {task.kind === 'incident' && <Badge color="owlRed" variant="filled">incident</Badge>}
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
