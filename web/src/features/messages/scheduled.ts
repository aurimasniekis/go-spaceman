import type { Message } from '@/api/client'

/** A scheduled message to control that is not yet due (ADR-0065): hidden from its recipient until deliver_at. */
export function pendingSchedule(m: Pick<Message, 'deliver_at' | 'state'>, now = Date.now()): boolean {
  if (!m.deliver_at || m.state !== 'stored') return false
  const t = Date.parse(m.deliver_at)
  return !Number.isNaN(t) && t > now
}

export type MessageView = 'people' | 'system' | 'scheduled' | 'all'
type Listed = Pick<Message, 'from_kind' | 'kind' | 'to_kind' | 'deliver_at' | 'state'>

/** A message SpaceMan sent itself (notifications to control), kept out of the people view. */
export const isSystem = (m: Pick<Message, 'from_kind' | 'kind'>) => m.from_kind === 'system' || m.kind === 'notification'

/**
 * The messages a view shows. The Scheduled view shows the server's pending list (`?scheduled=true`), so a reminder set
 * long ago in a busy space is still there; the others filter the recent messages.
 */
export function messagesInView<M extends Listed>(view: MessageView, recent: M[], scheduled: M[], onlyOperators: boolean): M[] {
  const base = view === 'scheduled' ? scheduled : recent.filter((m) => view === 'all' || (view === 'system') === isSystem(m))
  return base.filter((m) => !onlyOperators || m.to_kind === 'operators')
}
