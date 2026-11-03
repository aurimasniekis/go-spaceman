import type { HumanAction } from '@/api/client'
import type { NotificationPrefs } from './NotificationPrefs'

const rank: Record<string, number> = { low: 1, normal: 2, high: 3, urgent: 4 }

/** Actions this tab changed itself in the last few seconds (a claim): their changes are not announced back to it. */
const own = new Map<string, number>()
export function markOwnChange(actionId: string) {
  own.set(actionId, Date.now())
}
export function isOwnChange(actionId: string, now = Date.now()) {
  const at = own.get(actionId)
  return at !== undefined && now - at < 15_000
}

/** What a change to a human action means for one user, given what was announced before. */
export interface Announcement {
  key: string
  title: string
  body: string
}

/**
 * Decides which announcements a human action deserves (SM-HUI-006). `created` is true for a new action; `since` is when
 * this page started listening, so only events after it are announced; `seen` holds keys already announced. Pure, so
 * it is unit-tested.
 */
export function announcements(a: HumanAction, opts: { prefs: NotificationPrefs; me: string; created: boolean; since: number; seen: Set<string>; space: string }): Announcement[] {
  const { prefs, me, created, since, seen, space } = opts
  if (!prefs.enabled || (prefs.muted_spaces ?? []).includes(a.space_id)) return []
  const pending = a.status === 'open' || a.status === 'in_progress'
  const loud = (rank[a.priority] ?? 2) >= (rank[prefs.min_priority] ?? 1)
  const after = (t?: string) => !!t && Date.parse(t) >= since
  const out: Announcement[] = []
  const add = (key: string, title: string) => {
    if (!seen.has(key)) out.push({ key, title, body: `${a.title} · ${space}` })
  }
  const mine = a.requester_kind === 'operator' && a.requester_id === me
  if (created && pending && prefs.new_actions && loud && !mine) add(`${a.id}:new`, 'New human action')
  if (pending && prefs.assigned_to_me && me && a.assignee === me && after(a.updated_at) && !isOwnChange(a.id)) add(`${a.id}:assigned:${me}`, 'Assigned to you')
  if (pending && prefs.escalations && loud && after(a.escalated_at)) add(`${a.id}:escalated:${a.escalated_at}`, `Escalated to ${a.priority}`)
  if (pending && prefs.reminders && loud && after(a.reminded_at)) add(`${a.id}:reminded:${a.reminder_count}`, 'Reminder: still waiting')
  if (prefs.clarifications) {
    const last = (a.comments ?? []).at(-1)
    if (last && after(last.created_at) && last.author !== me) {
      // A reply from the requester, on an action this user asked for clarification on.
      if (last.kind === 'reply' && (a.comments ?? []).some((c) => c.kind === 'clarification' && c.author === me)) add(`${a.id}:reply:${last.id}`, 'The requester replied')
      // Someone asks this user, the requester, for more information.
      if (last.kind === 'clarification' && a.requester_kind === 'operator' && a.requester_id === me) add(`${a.id}:clarify:${last.id}`, 'More information requested')
    }
  }
  return out
}
