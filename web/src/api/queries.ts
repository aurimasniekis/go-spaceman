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
  connections: ['event-connections'] as const,
  templates: ['event-templates'] as const,
  template: (id: string) => ['event-template', id] as const,
  subscriptions: (space: string) => ['subscriptions', space] as const,
  queue: (space: string) => ['queue', space] as const,
  subscription: (space: string, id: string) => ['subscription', space, id] as const,
  inboxItem: (space: string, id: string) => ['inbox-item', space, id] as const,
  human: (space: string) => ['human', space] as const,
  humanPending: ['human', 'pending'] as const,
  accounts: ['accounts'] as const,
  accountKinds: ['account-kinds'] as const,
  spaceAccounts: (space: string) => ['space-accounts', space] as const,
  messages: (space: string) => ['messages', space] as const,
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

export const useConnections = () =>
  useQuery({ queryKey: keys.connections, queryFn: async () => unwrap(await api.GET('/event-connections')).items })

export const useTemplates = () => useQuery({ queryKey: keys.templates, queryFn: async () => unwrap(await api.GET('/event-templates')).items })

export const useTemplate = (id: string) =>
  useQuery({ queryKey: keys.template(id), queryFn: async () => unwrap(await api.GET('/event-templates/{template_id}', { params: { path: { template_id: id } } })) })

// Runtime state (running, error) comes from the ingestion manager without change-feed entries: refresh on a timer too.
export const useSubscriptions = (space: string) =>
  useQuery({
    queryKey: keys.subscriptions(space),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/subscriptions', { params: { path: { space_id: space } } })).items,
    refetchInterval: 10_000,
  })

export const useSubscription = (space: string, id: string) =>
  useQuery({
    queryKey: keys.subscription(space, id),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}', { params: { path: { space_id: space, subscription_id: id } } })),
    refetchInterval: 10_000,
  })

export const useDeliveries = (space: string, id: string) =>
  useQuery({
    queryKey: [...keys.subscription(space, id), 'deliveries'],
    queryFn: async () =>
      unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}/deliveries', { params: { path: { space_id: space, subscription_id: id }, query: { limit: 200 } } })).items,
    refetchInterval: 10_000,
  })

export const useQuarantine = (space: string, id: string) =>
  useQuery({
    queryKey: [...keys.subscription(space, id), 'quarantine'],
    queryFn: async () =>
      unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}/quarantine', { params: { path: { space_id: space, subscription_id: id } } })).items,
  })

export const useQueue = (space: string) =>
  useQuery({ queryKey: keys.queue(space), queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/tasks', { params: { path: { space_id: space } } })) })

export const useNext = (space: string) =>
  useQuery({
    queryKey: [...keys.queue(space), 'next'],
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/queue/next', { params: { path: { space_id: space } } })),
  })

export const useHumanActions = (space: string) =>
  useQuery({
    queryKey: keys.human(space),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/human-actions', { params: { path: { space_id: space } } })),
  })

/** Pending human actions across every space the principal can read, with counts (the nav badge). */
export const useHumanPending = (enabled = true) =>
  useQuery({ queryKey: keys.humanPending, enabled, queryFn: async () => unwrap(await api.GET('/human-actions')) })

export const useAccounts = (enabled = true) =>
  useQuery({ queryKey: keys.accounts, enabled, queryFn: async () => unwrap(await api.GET('/accounts')).items })

export const useAccountKinds = () =>
  useQuery({ queryKey: keys.accountKinds, staleTime: Infinity, queryFn: async () => unwrap(await api.GET('/account-kinds')).items })

/** The accounts a space's runs may use (names only, never secrets). */
export const useSpaceAccounts = (space: string) =>
  useQuery({
    queryKey: keys.spaceAccounts(space),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/accounts', { params: { path: { space_id: space } } })).items,
  })

/** A space's recent messages and how many wait for operators. */
export const useMessages = (space: string) =>
  useQuery({
    queryKey: keys.messages(space),
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/messages', { params: { path: { space_id: space } } })),
  })
