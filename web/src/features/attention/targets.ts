import type { Schemas } from '@/api/client'

export type Attention = Schemas['Attention']
export type AttentionItem = Schemas['AttentionItem']
export type AttentionTarget = Schemas['AttentionTarget']
export type AttentionSpace = Schemas['AttentionSpace']

/** A router destination for a target: a path with its params and search, typed per route. */
export type TargetRoute =
  | { to: '/spaces/$spaceId/runs/$runId'; params: { spaceId: string; runId: string } }
  | { to: '/spaces/$spaceId/queue'; params: { spaceId: string }; search: { task?: string } }
  | { to: '/spaces/$spaceId/events/$subscriptionId'; params: { spaceId: string; subscriptionId: string } }
  | { to: '/spaces/$spaceId/usage' | '/spaces/$spaceId/events' | '/spaces/$spaceId/limits' | '/spaces/$spaceId/cross-space' | '/spaces/$spaceId/human' | '/spaces/$spaceId'; params: { spaceId: string } }

/** Pages the Mini App does not have: their rows stay, unlinked, as the counts do. */
const WEB_ONLY: ReadonlyArray<AttentionTarget['type']> = ['subscription', 'space_usage', 'space_budget', 'xspace_request']

/** The route of an item's target. The server stores no URLs; null when there is nowhere to go (a Mini App web-only page, or a target missing its id). */
export function targetRoute(t: AttentionTarget, miniApp = false): TargetRoute | null {
  if (miniApp && WEB_ONLY.includes(t.type)) return null
  const params = { spaceId: t.space_id }
  switch (t.type) {
    case 'run': return t.id ? { to: '/spaces/$spaceId/runs/$runId', params: { ...params, runId: t.id } } : { to: '/spaces/$spaceId', params }
    case 'task': return { to: '/spaces/$spaceId/queue', params, search: t.id ? { task: t.id } : {} }
    case 'subscription': return t.id ? { to: '/spaces/$spaceId/events/$subscriptionId', params: { ...params, subscriptionId: t.id } } : { to: '/spaces/$spaceId/events', params }
    case 'space_usage': return { to: '/spaces/$spaceId/usage', params }
    case 'space_budget': return { to: '/spaces/$spaceId/limits', params }
    case 'xspace_request': return { to: '/spaces/$spaceId/cross-space', params }
    default: return null
  }
}

/** Short labels of the item kinds, for the kind filter and the rows. */
export const KIND_LABELS: Record<AttentionItem['kind'], string> = {
  task_blocked: 'Blocked task', task_failed: 'Failed task', quarantine: 'Quarantined event', run_unknown: 'Run unknown', run_failed: 'Run failed',
  budget_notice: 'Budget', run_parked: 'Parked run', account_limit: 'Account limit', account_switch_failed: 'Account switch failed', xspace_request: 'Cross-space request',
}

/** What the filters keep: a space ID and/or an item kind; empty means all. */
export type Filters = { space?: string; kind?: string }

/** One row of the page: a listed item, or the collapsed human actions. */
export type Row =
  | { type: 'item'; key: string; severity: 'act' | 'watch'; item: AttentionItem }
  | { type: 'human'; key: string; severity: 'act'; spaceId?: string; count: number; overdue: number; escalated: number }

export type Grouped = { act: Row[]; watch: Row[] }

const newest = (a: { since: string }, b: { since: string }) => Date.parse(b.since) - Date.parse(a.since)

/** Cuts an agent-supplied title down to a displayable length. Titles are untrusted text: the caller renders them as text. */
export function truncate(s: string, max = 120): string {
  const t = s.replace(/\s+/g, ' ').trim()
  return t.length > max ? `${t.slice(0, max - 1)}…` : t
}

/**
 * Groups the response into Act now and Watch. The rows come from `items[]` only (`spaces[]` may leave out a space whose
 * rows are all watch categories). Human actions are counted, not listed: they collapse into one row (per the space when
 * filtered to one, otherwise one for all spaces) that links to Needs you. A kind filter hides it unless it is `human`.
 */
export function groupAttention(data: Pick<Attention, 'items' | 'spaces'>, f: Filters = {}): Grouped {
  const items = data.items.filter((i) => (!f.space || i.space_id === f.space) && (!f.kind || i.kind === f.kind)).sort(newest)
  const rows: Row[] = items.map((item) => ({ type: 'item', key: `${item.kind}:${item.id}`, severity: item.severity, item }))
  const act = rows.filter((r) => r.severity === 'act')
  const watch = rows.filter((r) => r.severity === 'watch')
  if (!f.kind || f.kind === 'human') {
    const spaces = data.spaces.filter((s) => !f.space || s.space_id === f.space)
    const count = spaces.reduce((n, s) => n + (s.human_pending ?? 0), 0)
    if (count > 0) {
      act.unshift({
        type: 'human', key: `human:${f.space ?? 'all'}`, severity: 'act', spaceId: f.space, count,
        overdue: spaces.reduce((n, s) => n + (s.human_overdue ?? 0), 0), escalated: spaces.reduce((n, s) => n + (s.human_escalated ?? 0), 0),
      })
    }
  }
  return { act, watch }
}

/** The spaces to offer in the space filter: everything the items or the summary mention, by name, falling back to the ID. */
export function spaceOptions(data: Pick<Attention, 'items' | 'spaces'>): { value: string; label: string }[] {
  const names = new Map(data.spaces.map((s) => [s.space_id, s.name]))
  const ids = new Set([...data.spaces.map((s) => s.space_id), ...data.items.map((i) => i.space_id)])
  return [...ids].map((id) => ({ value: id, label: names.get(id) ?? id })).sort((a, b) => a.label.localeCompare(b.label))
}

/** The kinds to offer in the kind filter: those present, plus `human` when there are human actions. */
export function kindOptions(data: Pick<Attention, 'items' | 'spaces'>): { value: string; label: string }[] {
  const kinds = new Set<string>(data.items.map((i) => i.kind))
  const out = [...kinds].map((k) => ({ value: k, label: KIND_LABELS[k as AttentionItem['kind']] ?? k }))
  if (data.spaces.some((s) => (s.human_pending ?? 0) > 0)) out.push({ value: 'human', label: 'Human actions' })
  return out.sort((a, b) => a.label.localeCompare(b.label))
}

/** Items hidden by the 20-per-kind cap: the full count minus what is listed, per kind. */
export function truncatedKinds(data: Pick<Attention, 'items' | 'item_counts'>): { kind: string; shown: number; total: number }[] {
  const shown: Record<string, number> = {}
  for (const i of data.items) shown[i.kind] = (shown[i.kind] ?? 0) + 1
  return Object.entries(data.item_counts ?? {}).filter(([k, total]) => total > (shown[k] ?? 0)).map(([kind, total]) => ({ kind, shown: shown[kind] ?? 0, total }))
}
