import { Badge, Text } from '@mantine/core'
import { IconClock } from '@tabler/icons-react'
import type { Message } from '@/api/client'
import { ago, datetime } from '@/lib/format'
import { pendingSchedule } from './scheduled'

type Scheduled = Pick<Message, 'deliver_at' | 'state'>

/** The list row's "due in" badge of a scheduled message that is not yet due (ADR-0065). */
export function ScheduledBadge({ m, now }: { m: Scheduled; now?: number }) {
  if (!pendingSchedule(m, now)) return null
  return (
    <Badge color="owlYellow" variant="light" leftSection={<IconClock size={10} />} title={`Scheduled for ${datetime(m.deliver_at)}`}>
      {ago(m.deliver_at, now)}
    </Badge>
  )
}

/** The detail's "scheduled for" line: the recipient is not told before then. */
export function ScheduledNote({ m, now }: { m: Scheduled; now?: number }) {
  if (!m.deliver_at) return null
  const pending = pendingSchedule(m, now)
  return (
    <Text size="xs" fw={pending ? 600 : undefined} c="dimmed">
      {pending ? `Scheduled for ${datetime(m.deliver_at)} (${ago(m.deliver_at, now)}): control is not told before then` : `Scheduled for ${datetime(m.deliver_at)}`}
    </Text>
  )
}
