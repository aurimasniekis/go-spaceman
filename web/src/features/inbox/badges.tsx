import { Badge } from '@mantine/core'
import type { InboxItem } from '@/api/client'

const urgencyColor: Record<InboxItem['urgency'], string> = { low: 'gray', normal: 'owlBlue', high: 'owlYellow', urgent: 'owlRed' }
const statusColor: Record<InboxItem['status'], string> = { new: 'owlBlue', in_progress: 'owlPurple', deferred: 'owlYellow', rejected: 'gray', done: 'owlGreen' }
export const statusLabel: Record<InboxItem['status'], string> = { new: 'New', in_progress: 'In progress', deferred: 'Deferred', rejected: 'Rejected', done: 'Done' }
export const sourceLabel: Record<InboxItem['source_kind'], string> = {
  operator: 'Operator', worker: 'Worker', human_response: 'Human response', dreamer: 'Dreamer', kb: 'Knowledge base',
  cross_space: 'Another space', event: 'Event', webhook: 'Webhook',
}

export function UrgencyBadge({ urgency }: { urgency: InboxItem['urgency'] }) {
  if (urgency === 'normal') return null
  return <Badge color={urgencyColor[urgency]} variant={urgency === 'urgent' ? 'filled' : 'light'}>{urgency}</Badge>
}

export function InboxStatusBadge({ status }: { status: InboxItem['status'] }) {
  return <Badge color={statusColor[status]} variant="light">{statusLabel[status]}</Badge>
}
