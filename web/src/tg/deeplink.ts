import { webApp } from './webapp'

/**
 * Deep links (plan §4.6): a Mini App opened as t.me/<bot>/<app>?startapp=<hint> carries the hint as initData's
 * start_param. It is a routing hint and authorizes nothing: the page it names still needs the session's grants. The
 * hint is `<kind>-<space>[-<id>]`, ASCII only (IDs are lower-case base-32 with underscores, so `-` never occurs in
 * one): `space-spc_x`, `inbox-spc_x-inb_y`, `task-spc_x-tsk_y`, `human-spc_x-hum_y`, `run-spc_x-run_y`, and the spaceless
 * `attention` and `needs` (the cross-space "Needs you" page).
 */
const ID = /^[a-z]{2,5}_[0-9a-z]{1,40}$/

export type DeepLink = { to: string; params?: Record<string, string>; search?: Record<string, string> }

export function parseStartParam(raw: string | undefined | null): DeepLink | null {
  if (!raw || raw.length > 200 || !/^[A-Za-z0-9_-]+$/.test(raw)) return null
  const [kind, space, id, ...rest] = raw.split('-')
  if (rest.length > 0) return null
  if (kind === 'attention') return space === undefined ? { to: '/attention' } : null
  if (kind === 'needs') return space === undefined ? { to: '/human' } : null
  if (!space || !ID.test(space)) return null
  const spaceId = space
  switch (kind) {
    case 'space':
      return id === undefined ? { to: '/spaces/$spaceId', params: { spaceId } } : null
    case 'inbox':
      return id && ID.test(id) ? { to: '/spaces/$spaceId/inbox/$itemId', params: { spaceId, itemId: id } } : null
    case 'run':
      return id && ID.test(id) ? { to: '/spaces/$spaceId/runs/$runId', params: { spaceId, runId: id } } : null
    case 'task':
      // The queue page opens a task's drawer from ?task=.
      return id && ID.test(id) ? { to: '/spaces/$spaceId/queue', params: { spaceId }, search: { task: id } } : null
    case 'human':
      return id && ID.test(id) ? { to: '/spaces/$spaceId/human', params: { spaceId }, search: { action: id } } : null
    default:
      return null
  }
}

/** The WebApp's start parameter: initDataUnsafe's, or the one inside initData (the signed one). */
export function startParam(): string | undefined {
  const app = webApp()
  if (!app) return undefined
  try {
    return new URLSearchParams(app.initData).get('start_param') ?? app.initDataUnsafe?.start_param ?? undefined
  } catch {
    return undefined
  }
}
