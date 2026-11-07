import type { Message } from '@/api/client'

/** A scheduled message to control that is not yet due (ADR-0065): hidden from its recipient until deliver_at. */
export function pendingSchedule(m: Pick<Message, 'deliver_at' | 'state'>, now = Date.now()): boolean {
  if (!m.deliver_at || m.state !== 'stored') return false
  const t = Date.parse(m.deliver_at)
  return !Number.isNaN(t) && t > now
}
