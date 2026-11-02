import { keepPreviousData, queryOptions, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { api, unwrap } from './client'
import { setSession } from './session'

/** Query keys, shared by the watch feed's invalidation. */
export const keys = {
  me: ['me'] as const,
  system: ['system'] as const,
  spaces: ['spaces'] as const,
  space: (id: string) => ['space', id] as const,
  runs: (space: string) => ['runs', space] as const,
  run: (space: string, run: string) => ['run', space, run] as const,
  nodes: ['nodes'] as const,
  node: (id: string) => ['node', id] as const,
  users: ['users'] as const,
  tokens: ['tokens'] as const,
  allTokens: ['tokens', 'all'] as const,
  sessions: ['sessions'] as const,
  audit: (f: AuditFilter) => ['audit', f] as const,
  rootToken: ['root-token'] as const,
  inbox: (space: string) => ['inbox', space] as const,
  inboxItem: (space: string, id: string) => ['inbox-item', space, id] as const,
}

export interface AuditFilter {
  cursor?: string
  action?: string
  target_id?: string
  actor_id?: string
}

export const meQuery = queryOptions({
  queryKey: keys.me,
  queryFn: async () => {
    const res = await api.GET('/auth/me')
    if (res.response.status === 401) {
      setSession('')
      return null
    }
    const me = unwrap(res)
    setSession(me.csrf_token, me.limited)
    return me
  },
  staleTime: 30_000,
})

export const useMe = () => useQuery(meQuery)

export const systemQuery = queryOptions({
  queryKey: keys.system,
  queryFn: async () => unwrap(await api.GET('/system/info')),
  staleTime: 10_000,
})

export const useSystem = () => useQuery(systemQuery)

export const useSpaces = () =>
  useQuery({ queryKey: keys.spaces, queryFn: async () => unwrap(await api.GET('/spaces')).items })

export const useSpace = (id: string) =>
  useQuery({
    queryKey: keys.space(id),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}', { params: { path: { space_id: id } } })),
    // Lease expiry is time-based (no change entry), so the lease shown is refreshed on a timer as well.
    refetchInterval: 15_000,
  })

export const useRuns = (space: string) =>
  useQuery({
    queryKey: keys.runs(space),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/runs', { params: { path: { space_id: space }, query: { limit: 200 } } })).items,
  })

export const useRun = (space: string, run: string) =>
  useQuery({
    queryKey: keys.run(space, run),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/runs/{run_id}', { params: { path: { space_id: space, run_id: run } } })),
  })

// Node connectivity (connected, last_seen_at) changes without change-feed entries, so the list also refreshes on a timer.
export const useNodes = () =>
  useQuery({ queryKey: keys.nodes, queryFn: async () => unwrap(await api.GET('/nodes')).items, refetchInterval: 15_000 })

export const useUsers = () => useQuery({ queryKey: keys.users, queryFn: async () => unwrap(await api.GET('/users')).items })

export const useMyTokens = () => useQuery({ queryKey: keys.tokens, queryFn: async () => unwrap(await api.GET('/tokens')).items })

export const useAllTokens = (enabled: boolean) =>
  useQuery({ queryKey: keys.allTokens, queryFn: async () => unwrap(await api.GET('/admin/tokens')).items, enabled })

export const useSessions = () =>
  useQuery({ queryKey: keys.sessions, queryFn: async () => unwrap(await api.GET('/auth/sessions')).items })

export const useAudit = (f: AuditFilter) =>
  useQuery({
    queryKey: keys.audit(f),
    queryFn: async () =>
      unwrap(
        await api.GET('/audit', {
          params: { query: { limit: 100, cursor: f.cursor || undefined, action: f.action || undefined, target_id: f.target_id || undefined, actor_id: f.actor_id || undefined } },
        }),
      ),
    placeholderData: keepPreviousData,
  })

export type InboxStatus = 'new' | 'in_progress' | 'deferred' | 'rejected' | 'done'

export const useInbox = (space: string, status: InboxStatus | '') =>
  useInfiniteQuery({
    queryKey: [...keys.inbox(space), status],
    initialPageParam: '',
    queryFn: async ({ pageParam }) =>
      unwrap(await api.GET('/spaces/{space_id}/inbox', { params: { path: { space_id: space }, query: { status: status || undefined, cursor: pageParam || undefined, limit: 50 } } })),
    getNextPageParam: (last) => last.next_cursor || undefined,
  })

export const useInboxItem = (space: string, id: string) =>
  useQuery({
    queryKey: keys.inboxItem(space, id),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/inbox/{item_id}', { params: { path: { space_id: space, item_id: id } } })),
  })
