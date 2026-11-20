import type { AttentionItem } from '@/api/client'

export type ItemLink =
  | { to: '/spaces/$spaceId/queue'; params: { spaceId: string } }
  | { to: '/spaces/$spaceId/runs/$runId'; params: { spaceId: string; runId: string } }
  | { to: '/spaces/$spaceId/events/$subscriptionId'; params: { spaceId: string; subscriptionId: string } }
  | { to: '/spaces/$spaceId/limits'; params: { spaceId: string } }
  | { to: '/spaces/$spaceId/usage'; params: { spaceId: string } }
  | { to: '/spaces/$spaceId/cross-space'; params: { spaceId: string } }
  | { to: '/spaces/$spaceId'; params: { spaceId: string } }

/** Where an attention item is handled. The server stores no URLs: the target's type and id decide (plan §2.3). */
export function itemLink(it: AttentionItem): ItemLink {
  const spaceId = it.target.space_id || it.space_id
  const id = it.target.id
  switch (it.target.type) {
    case 'run': return id ? { to: '/spaces/$spaceId/runs/$runId', params: { spaceId, runId: id } } : { to: '/spaces/$spaceId', params: { spaceId } }
    case 'task': return { to: '/spaces/$spaceId/queue', params: { spaceId } }
    case 'subscription': return id ? { to: '/spaces/$spaceId/events/$subscriptionId', params: { spaceId, subscriptionId: id } } : { to: '/spaces/$spaceId', params: { spaceId } }
    case 'space_budget': return { to: '/spaces/$spaceId/limits', params: { spaceId } }
    case 'space_usage': return { to: '/spaces/$spaceId/usage', params: { spaceId } }
    case 'xspace_request': return { to: '/spaces/$spaceId/cross-space', params: { spaceId } }
  }
}

/** Act before watch, then newest first; the server's order within a severity is kept stable by `since`. */
export function topItems(items: AttentionItem[], n = 5): AttentionItem[] {
  const rank = (i: AttentionItem) => (i.severity === 'act' ? 0 : 1)
  return [...items].sort((a, b) => rank(a) - rank(b) || Date.parse(b.since) - Date.parse(a.since)).slice(0, n)
}
