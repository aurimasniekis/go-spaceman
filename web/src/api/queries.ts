import { keepPreviousData, queryOptions, useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { api, unwrap } from './client'
import { setSession } from './session'

/** Query keys, shared by the watch feed's invalidation. */
/**
 * How long data the change feed doesn't cover (sessions, audit, event connections, SSH keys, role options) stays fresh:
 * the default (router.tsx) is long because the feed invalidates what changes, which it can't do for these.
 */
export const UNWATCHED = 5_000

export const keys = {
  me: ['me'] as const,
  system: ['system'] as const,
  systemStatus: ['system', 'status'] as const,
  spaces: ['spaces'] as const,
  space: (id: string) => ['space', id] as const,
  runs: (space: string) => ['runs', space] as const,
  relocation: (space: string) => ['relocation', space] as const,
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
  outbound: ['outbound-events'] as const,
  backups: ['backups'] as const,
  restore: ['restore'] as const,
  templates: ['event-templates'] as const,
  template: (id: string) => ['event-template', id] as const,
  subscriptions: (space: string) => ['subscriptions', space] as const,
  queue: (space: string) => ['queue', space] as const,
  subscription: (space: string, id: string) => ['subscription', space, id] as const,
  inboxItem: (space: string, id: string) => ['inbox-item', space, id] as const,
  human: (space: string) => ['human', space] as const,
  humanPending: ['human', 'pending'] as const,
  accounts: ['accounts'] as const,
  gitKeys: ['git-ssh-keys'] as const,
  harborPilot: ['harbor-pilot'] as const,
  harborPilotGateway: ['harbor-pilot', 'gateway'] as const,
  spaceHarborPilot: (space: string) => ['space-harbor-pilot', space] as const,
  spaceHarborPilotStatus: (space: string) => ['space-harbor-pilot', space, 'status'] as const,
  spaceGitKeys: (space: string) => ['git-ssh-keys', 'space', space] as const,
  accountKinds: ['account-kinds'] as const,
  spaceAccounts: (space: string) => ['space-accounts', space] as const,
  messages: (space: string) => ['messages', space] as const,
  /** Under keys.messages, so invalidating a space's messages refreshes it too. */
  scheduledMessages: (space: string) => ['messages', space, 'scheduled'] as const,
  prompts: ['prompts'] as const,
  prompt: (id: string) => ['prompt', id] as const,
  profiles: ['harness-profiles'] as const,
  roleTemplates: ['role-templates'] as const,
  spaceRoles: (space: string) => ['space-roles', space] as const,
  roleOptions: (space: string) => ['role-options', space] as const,
  humanTypes: ['human-types'] as const,
  skills: ['skills'] as const,
  mcpServers: ['mcp-servers'] as const,
  approvalPolicies: ['approval-policies'] as const,
  memory: (spaceId: string) => ['memory', spaceId] as const,
  kbs: (spaceId: string) => ['kbs', spaceId] as const,
  xspaceRequests: (space: string) => ['xspace-requests', space] as const,
  xspacePeers: (space: string) => ['xspace-peers', space] as const,
  xspaceSettings: (space: string) => ['xspace-settings', space] as const,
  xspaceAll: ['xspace-all'] as const,
  dreams: (spaceId: string) => ['dreams', spaceId] as const,
  mcpGrants: (id: string) => ['mcp-grants', id] as const,
  spaceMcp: (id: string) => ['space-mcp', id] as const,
  spacemanTools: (preset: string, tools: string[], grants: string[]) => ['spaceman-tools', preset, tools, grants] as const,
  spaceHumanTypes: (space: string) => ['space-human-types', space] as const,
  /** Human-action policies of a space, or the installation defaults for ''. */
  humanPolicies: (space: string) => ['human-policies', space] as const,
  signInProviders: ['sign-in-providers'] as const,
  oidcProviders: ['oidc-providers'] as const,
  oidcMappings: (id: string) => ['oidc-mappings', id] as const,
  myIdentities: ['identities', 'me'] as const,
  limits: (space: string) => ['limits', space] as const,
  usageLimits: (space: string) => ['usage-limits', space] as const,
  limitWaits: (space: string) => ['limit-waits', space] as const,
  schedules: (space: string) => ['schedules', space] as const,
  schedule: (space: string, id: string) => ['schedules', space, id] as const,
  repositories: (space: string) => ['repositories', space] as const,
  userIdentities: (id: string) => ['identities', id] as const,
  attention: (space = '') => ['attention', space] as const,
  humanSearch: (space: string, f: HumanQuery) => ['human', 'search', space, f] as const,
  notificationPrefs: ['notification-prefs'] as const,
}

/** Search, filter and sort parameters of the human-action listings (SM-HUM-006, SM-HUI-001). */
export interface HumanQuery {
  q?: string
  type?: string
  status?: string
  assignee?: string
  priority?: string
  requester?: string
  due?: string
  sort?: string
  space?: string
  pending?: boolean
}

function humanParams(f: HumanQuery) {
  const out: Record<string, string | boolean> = {}
  for (const [k, v] of Object.entries(f)) if (v !== undefined && v !== '' && v !== false) out[k] = v as string | boolean
  return out
}

export interface AuditFilter {
  cursor?: string
  action?: string
  target_id?: string
  actor_id?: string
}

export const meQuery = queryOptions({
  queryKey: keys.me,
  queryFn: async ({ signal }) => {
    const res = await api.GET('/auth/me', { signal })
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
  queryFn: async ({ signal }) => unwrap(await api.GET('/system/info', { signal })),
  staleTime: 10_000,
})

export const useSystem = () => useQuery(systemQuery)

/** This instance's status (system:read, admins; the caller gates it with `enabled`), refreshed while the page is open. */
export const useSystemStatus = (enabled: boolean) =>
  useQuery({ queryKey: keys.systemStatus, queryFn: async ({ signal }) => unwrap(await api.GET('/system', { signal })), enabled, refetchInterval: 15_000 })

export const useSpaces = () =>
  useQuery({ queryKey: keys.spaces, queryFn: async ({ signal }) => unwrap(await api.GET('/spaces', { signal })).items })

export const useSpace = (id: string) =>
  useQuery({
    queryKey: keys.space(id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}', { params: { path: { space_id: id } }, signal })),
    // Lease expiry is time-based (no change entry), so the lease shown is refreshed on a timer as well.
    refetchInterval: 15_000,
  })

/** A space's runs (newest 200); also used for every space at once by the Terminals workspace. */
export const runsQuery = (space: string) =>
  queryOptions({
    queryKey: keys.runs(space),
    // The signal aborts a fetch nobody waits for any more (the view unmounted), instead of leaving it open.
    queryFn: async ({ signal }) =>
      unwrap(await api.GET('/spaces/{space_id}/runs', { params: { path: { space_id: space }, query: { limit: 200 } }, signal })).items,
  })

export const useRuns = (space: string) => useQuery(runsQuery(space))

/** The space's relocation in progress, or its latest one (null when it never had one). */
export const useRelocation = (space: string) =>
  useQuery({
    queryKey: keys.relocation(space),
    queryFn: async ({ signal }) => {
      const res = await api.GET('/spaces/{space_id}/relocation', { params: { path: { space_id: space } }, signal })
      if (res.response.status === 404) return null
      return unwrap(res)
    },
  })

export const useRun = (space: string, run: string) =>
  useQuery({
    queryKey: keys.run(space, run),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/runs/{run_id}', { params: { path: { space_id: space, run_id: run } }, signal })),
  })

// Node connectivity (connected, last_seen_at) changes without change-feed entries, so the list also refreshes on a timer.
export const useNodes = () =>
  useQuery({ queryKey: keys.nodes, queryFn: async ({ signal }) => unwrap(await api.GET('/nodes', { signal })).items, refetchInterval: 15_000 })

export const useUsers = () => useQuery({ queryKey: keys.users, queryFn: async ({ signal }) => unwrap(await api.GET('/users', { signal })).items })

export const useMyTokens = () => useQuery({ queryKey: keys.tokens, queryFn: async ({ signal }) => unwrap(await api.GET('/tokens', { signal })).items })

export const useAllTokens = (enabled: boolean) =>
  useQuery({ queryKey: keys.allTokens, queryFn: async ({ signal }) => unwrap(await api.GET('/admin/tokens', { signal })).items, enabled })

export const useSessions = () =>
  useQuery({ queryKey: keys.sessions, queryFn: async ({ signal }) => unwrap(await api.GET('/auth/sessions', { signal })).items, staleTime: UNWATCHED })

export const useAudit = (f: AuditFilter) =>
  useQuery({
    queryKey: keys.audit(f),
    queryFn: async ({ signal }) =>
      unwrap(
        await api.GET('/audit', {
          params: { query: { limit: 100, cursor: f.cursor || undefined, action: f.action || undefined, target_id: f.target_id || undefined, actor_id: f.actor_id || undefined } }, signal,
        }),
      ),
    placeholderData: keepPreviousData,
    staleTime: UNWATCHED,
  })

export type InboxStatus = 'new' | 'in_progress' | 'deferred' | 'rejected' | 'done'

export const useInbox = (space: string, status: InboxStatus | '') =>
  useInfiniteQuery({
    queryKey: [...keys.inbox(space), status],
    initialPageParam: '',
    queryFn: async ({ pageParam, signal }) =>
      unwrap(await api.GET('/spaces/{space_id}/inbox', { params: { path: { space_id: space }, query: { status: status || undefined, cursor: pageParam || undefined, limit: 50 } }, signal })),
    getNextPageParam: (last) => last.next_cursor || undefined,
  })

export const useInboxItem = (space: string, id: string) =>
  useQuery({
    queryKey: keys.inboxItem(space, id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/inbox/{item_id}', { params: { path: { space_id: space, item_id: id } }, signal })),
  })

export const useConnections = () =>
  useQuery({ queryKey: keys.connections, queryFn: async ({ signal }) => unwrap(await api.GET('/event-connections', { signal })).items, staleTime: UNWATCHED })

/** Backups and the schedule (global admins only). The change feed refreshes it while a backup or verification runs. */
export const useBackups = (enabled: boolean) =>
  useQuery({ queryKey: keys.backups, queryFn: async ({ signal }) => unwrap(await api.GET('/system/backups', { signal })), enabled })

/** The restore notice: whether this installation was restored from a backup (any signed-in principal). */
export const useRestoreNotice = (enabled: boolean) =>
  useQuery({ queryKey: keys.restore, queryFn: async ({ signal }) => unwrap(await api.GET('/system/restore', { signal })), enabled, staleTime: 60_000 })

/** Outbound publication status (global admins only; the caller gates it with `enabled`). */
export const useOutbound = (enabled: boolean) =>
  useQuery({ queryKey: keys.outbound, queryFn: async ({ signal }) => unwrap(await api.GET('/system/outbound-events', { signal })), enabled, refetchInterval: 15_000 })

export const useTemplates = () => useQuery({ queryKey: keys.templates, queryFn: async ({ signal }) => unwrap(await api.GET('/event-templates', { signal })).items })

export const useTemplate = (id: string) =>
  useQuery({ queryKey: keys.template(id), queryFn: async ({ signal }) => unwrap(await api.GET('/event-templates/{template_id}', { params: { path: { template_id: id } }, signal })) })

// Runtime state (running, error) comes from the ingestion manager without change-feed entries: refresh on a timer too.
export const useSubscriptions = (space: string) =>
  useQuery({
    queryKey: keys.subscriptions(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/subscriptions', { params: { path: { space_id: space } }, signal })).items,
    refetchInterval: 10_000,
  })

export const useSubscription = (space: string, id: string) =>
  useQuery({
    queryKey: keys.subscription(space, id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}', { params: { path: { space_id: space, subscription_id: id } }, signal })),
    refetchInterval: 10_000,
  })

export const useDeliveries = (space: string, id: string) =>
  useQuery({
    queryKey: [...keys.subscription(space, id), 'deliveries'],
    queryFn: async ({ signal }) =>
      unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}/deliveries', { params: { path: { space_id: space, subscription_id: id }, query: { limit: 200 } }, signal })).items,
    refetchInterval: 10_000,
  })

export const useReplays = (space: string, id: string) =>
  useQuery({
    queryKey: [...keys.subscription(space, id), 'replays'],
    queryFn: async ({ signal }) =>
      unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}/replays', { params: { path: { space_id: space, subscription_id: id } }, signal })).items ?? [],
  })

/** A space's effective limits with their sources and its budget state (SM-LIMIT-001). */
export const useLimits = (space: string) =>
  useQuery({
    queryKey: keys.limits(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/limits', { params: { path: { space_id: space } }, signal })),
  })

/** A space's usage-limit holds, its own (cleared ones too) then other spaces' open ones on its accounts (ADR-0079). */
export const useUsageLimits = (space: string) =>
  useQuery({
    queryKey: keys.usageLimits(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/usage-limits', { params: { path: { space_id: space } }, signal })).items ?? [],
  })

/** A space's runs parked at a usage limit, and what became of them (ADR-0079). */
export const useLimitWaits = (space: string) =>
  useQuery({
    queryKey: keys.limitWaits(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/limit-waits', { params: { path: { space_id: space } }, signal })).items ?? [],
  })

export const useSchedules = (space: string) =>
  useQuery({
    queryKey: keys.schedules(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/schedules', { params: { path: { space_id: space } }, signal })).items ?? [],
  })

export const useSchedule = (space: string, id: string | null) =>
  useQuery({
    queryKey: keys.schedule(space, id ?? ''),
    enabled: !!id,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/schedules/{schedule_id}', { params: { path: { space_id: space, schedule_id: id! } }, signal })),
  })

/** A space's configured repositories with the node's latest report (ADR-0044). */
export const useRepositories = (space: string) =>
  useQuery({
    queryKey: keys.repositories(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/repositories', { params: { path: { space_id: space } }, signal })),
  })

export const useQuarantine = (space: string, id: string) =>
  useQuery({
    queryKey: [...keys.subscription(space, id), 'quarantine'],
    queryFn: async ({ signal }) =>
      unwrap(await api.GET('/spaces/{space_id}/subscriptions/{subscription_id}/quarantine', { params: { path: { space_id: space, subscription_id: id } }, signal })).items,
  })

export const useQueue = (space: string) =>
  useQuery({ queryKey: keys.queue(space), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/tasks', { params: { path: { space_id: space } }, signal })) })

export const useNext = (space: string) =>
  useQuery({
    queryKey: [...keys.queue(space), 'next'],
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/queue/next', { params: { path: { space_id: space } }, signal })),
  })

export const useHumanActions = (space: string) =>
  useQuery({
    queryKey: keys.human(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/human-actions', { params: { path: { space_id: space } }, signal })),
  })

/** A space's human actions, searched and filtered on the server; the previous result stays while a new one loads. */
export const useHumanSearch = (space: string, f: HumanQuery) =>
  useQuery({
    queryKey: keys.humanSearch(space, f),
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/human-actions', {
      params: { path: { space_id: space }, query: humanParams(f) as never }, signal,
    })),
  })

/** Pending human actions across your spaces, searched and filtered on the server (the Needs you page). */
export const useHumanPendingSearch = (f: HumanQuery) =>
  useQuery({
    queryKey: keys.humanSearch('', f),
    placeholderData: keepPreviousData,
    queryFn: async ({ signal }) => unwrap(await api.GET('/human-actions', { params: { query: humanParams(f) as never }, signal })),
  })

/** What needs a person (SM-UI-003): across your spaces, or one space. Refreshed by the change feed. */
export const useAttention = (space = '', enabled = true) =>
  useQuery({
    queryKey: keys.attention(space),
    enabled,
    queryFn: async ({ signal }) => unwrap(await api.GET('/attention', { params: { query: space ? { space } : {} }, signal })),
  })

/** Your notification preferences (SM-HUI-006); version 0 means the defaults. */
export const useNotificationPrefs = (enabled = true) =>
  useQuery({ queryKey: keys.notificationPrefs, enabled, queryFn: async ({ signal }) => unwrap(await api.GET('/auth/me/notification-preferences', { signal })) })

/** Pending human actions across every space the principal can read, with counts (the nav badge). */
export const useHumanPending = (enabled = true) =>
  useQuery({ queryKey: keys.humanPending, enabled, queryFn: async ({ signal }) => unwrap(await api.GET('/human-actions', { signal })) })

export const useAccounts = (enabled = true) =>
  useQuery({ queryKey: keys.accounts, enabled, queryFn: async ({ signal }) => unwrap(await api.GET('/accounts', { signal })).items })

export const useAccountKinds = () =>
  useQuery({ queryKey: keys.accountKinds, staleTime: Infinity, queryFn: async ({ signal }) => unwrap(await api.GET('/account-kinds', { signal })).items })

/** The accounts a space's runs may use (names only, never secrets). */
export const useSpaceAccounts = (space: string) =>
  useQuery({
    queryKey: keys.spaceAccounts(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/accounts', { params: { path: { space_id: space } }, signal })).items,
  })

/** A space's recent messages and how many wait for operators. */
export const useMessages = (space: string) =>
  useQuery({
    queryKey: keys.messages(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/messages', { params: { path: { space_id: space } }, signal })),
  })

/** A space's pending scheduled messages to control, earliest first: the server's list, not a filter of the newest messages (ADR-0065). */
export const useScheduledMessages = (space: string) =>
  useQuery({
    queryKey: keys.scheduledMessages(space),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/messages', { params: { path: { space_id: space }, query: { scheduled: true } }, signal })).items,
  })

export const usePrompts = () => useQuery({ queryKey: keys.prompts, queryFn: async ({ signal }) => unwrap(await api.GET('/prompts', { signal })).items })
export const usePrompt = (id: string) =>
  useQuery({ queryKey: keys.prompt(id), queryFn: async ({ signal }) => unwrap(await api.GET('/prompts/{prompt_id}', { params: { path: { prompt_id: id } }, signal })) })
export const useProfiles = () => useQuery({ queryKey: keys.profiles, queryFn: async ({ signal }) => unwrap(await api.GET('/harness-profiles', { signal })).items })
export const useRoleTemplates = () => useQuery({ queryKey: keys.roleTemplates, queryFn: async ({ signal }) => unwrap(await api.GET('/role-templates', { signal })).items })
export const useSpaceRoles = (space: string) =>
  useQuery({ queryKey: keys.spaceRoles(space), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/roles', { params: { path: { space_id: space } }, signal })).items })
export const useRoleOptions = (space: string, enabled = true) =>
  useQuery({ queryKey: keys.roleOptions(space), enabled, staleTime: UNWATCHED,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/role-options', { params: { path: { space_id: space } }, signal })) })

export const useHumanTypes = (enabled = true) => useQuery({ queryKey: keys.humanTypes, enabled, queryFn: async ({ signal }) => unwrap(await api.GET('/human-types', { signal })).items })
export const useSpaceHumanTypes = (space: string) =>
  useQuery({ queryKey: keys.spaceHumanTypes(space), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/human-types', { params: { path: { space_id: space } }, signal })).items })

export const useSkills = () => useQuery({ queryKey: keys.skills, queryFn: async ({ signal }) => unwrap(await api.GET('/skills', { signal })).items })
export const useMcpServers = () => useQuery({ queryKey: keys.mcpServers, queryFn: async ({ signal }) => unwrap(await api.GET('/mcp-servers', { signal })).items })
export const useMcpGrants = (id: string) =>
  useQuery({ queryKey: keys.mcpGrants(id), queryFn: async ({ signal }) => unwrap(await api.GET('/mcp-servers/{server_id}/grants', { params: { path: { server_id: id } }, signal })).items })
/** The SpaceMan MCP tool picker (ADR-0063): every tool, marked with what a preset, named tools and grants make of it. */
export const useSpacemanTools = (preset: string, tools: string[], grants: string[], enabled = true) =>
  useQuery({
    queryKey: keys.spacemanTools(preset, tools, grants),
    queryFn: async ({ signal }) =>
      unwrap(await api.GET('/mcp-servers/spaceman/tools', {
        params: { query: { preset: preset as 'read' | 'operate' | 'admin' | 'custom', tools: tools.length ? tools : undefined, grants: grants.length ? grants : undefined } },
        // The API reads comma-separated lists (tools=a,b).
        querySerializer: { array: { style: 'form', explode: false } }, signal,
      })),
    enabled,
    placeholderData: (prev) => prev,
    staleTime: 60_000,
  })
export const useSpaceMcp = (spaceId: string) =>
  useQuery({ queryKey: keys.spaceMcp(spaceId), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/mcp-servers', { params: { path: { space_id: spaceId } }, signal })).items })
export const useApprovalPolicies = () => useQuery({ queryKey: keys.approvalPolicies, queryFn: async ({ signal }) => unwrap(await api.GET('/approval-policies', { signal })).items })
export const useMemory = (spaceId: string) =>
  useQuery({ queryKey: keys.memory(spaceId), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/memory', { params: { path: { space_id: spaceId } }, signal })) })
export const useKBs = (spaceId: string) =>
  useQuery({ queryKey: keys.kbs(spaceId), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/kbs', { params: { path: { space_id: spaceId } }, signal })).items })
export const useKB = (spaceId: string, id: string | null) =>
  useQuery({ queryKey: [...keys.kbs(spaceId), id], enabled: !!id,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/kbs/{kb_id}', { params: { path: { space_id: spaceId, kb_id: id! } }, signal })) })
export const useDreams = (spaceId: string) =>
  useQuery({ queryKey: keys.dreams(spaceId), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/dreams', { params: { path: { space_id: spaceId } }, signal })).items })

/** Enabled sign-in providers (public: the sign-in page shows one button each). */
export const useSignInProviders = () =>
  useQuery({ queryKey: keys.signInProviders, queryFn: async ({ signal }) => unwrap(await api.GET('/auth/oidc/providers', { signal })).items, staleTime: 30_000 })

/** OIDC providers with the redirect URI to register at them (client secrets are never returned). */
export const useOidcProviders = () => useQuery({ queryKey: keys.oidcProviders, queryFn: async ({ signal }) => unwrap(await api.GET('/oidc/providers', { signal })) })

export const useOidcMappings = (id: string) =>
  useQuery({
    queryKey: keys.oidcMappings(id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/oidc/providers/{provider_id}/mappings', { params: { path: { provider_id: id } }, signal })).items,
  })

export const useMyIdentities = () => useQuery({ queryKey: keys.myIdentities, queryFn: async ({ signal }) => unwrap(await api.GET('/auth/identities', { signal })).items })

export const useUserIdentities = (id: string) =>
  useQuery({
    queryKey: keys.userIdentities(id),
    queryFn: async ({ signal }) => unwrap(await api.GET('/users/{user_id}/identities', { params: { path: { user_id: id } }, signal })).items,
  })

/** A space's cross-space requests, outgoing and incoming (ADR-0038). */
export const useXSpaceRequests = (spaceId: string) =>
  useQuery({ queryKey: keys.xspaceRequests(spaceId),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/xspace/requests', { params: { path: { space_id: spaceId }, query: { limit: 200 } }, signal })).items })
/** What a space accepts from and may send to each peer. */
export const useXSpacePeers = (spaceId: string) =>
  useQuery({ queryKey: keys.xspacePeers(spaceId),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/xspace/peers', { params: { path: { space_id: spaceId } }, signal })).items })
export const useXSpaceSettings = (spaceId: string) =>
  useQuery({ queryKey: keys.xspaceSettings(spaceId),
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/xspace/settings', { params: { path: { space_id: spaceId } }, signal })) })
/** Every cross-space request and peer configuration (global administrators). */
export const useAllXSpace = (enabled: boolean) =>
  useQuery({ queryKey: keys.xspaceAll, enabled,
    queryFn: async ({ signal }) => {
      const [requests, peers] = await Promise.all([api.GET('/xspace/requests', { params: { query: { limit: 200 } }, signal }), api.GET('/xspace/peers', { signal })])
      return { requests: unwrap(requests).items, peers: unwrap(peers).items }
    } })

/** Shared Git SSH keys (ADR-0058): administrators manage them. */
export const useGitKeys = (enabled = true) =>
  useQuery({ queryKey: keys.gitKeys, enabled, staleTime: UNWATCHED, queryFn: async ({ signal }) => unwrap(await api.GET('/git-ssh-keys', { signal })).items })

/** The Harbor Pilot connection (ADR-0060; administrators). null when none is configured. */
export const useHarborPilot = (enabled = true) =>
  useQuery({ queryKey: keys.harborPilot, enabled, queryFn: async ({ signal }) => {
    const r = await api.GET('/harbor-pilot/connection', { signal })
    if (r.response.status === 404) return null
    return unwrap(r)
  } })

/** The connected Harbor Pilot's version (GET /api/v1/system on 0.1.4 or later; legacy before). Under keys.harborPilot, so a connection change refreshes it. */
export const useHarborPilotGateway = (enabled = true) =>
  useQuery({ queryKey: keys.harborPilotGateway, enabled, queryFn: async ({ signal }) => unwrap(await api.GET('/harbor-pilot/gateway', { signal })) })

/** A space's Harbor Pilot settings (the defaults when it never opted in). */
export const useSpaceHarborPilot = (space: string) =>
  useQuery({ queryKey: keys.spaceHarborPilot(space), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/harbor-pilot', { params: { path: { space_id: space } }, signal })) })

/** A space's Harbor Pilot budget status and project usage (HP-T5). Central reuses a read for 30 s. */
export const useHarborPilotStatus = (space: string) =>
  useQuery({ queryKey: keys.spaceHarborPilotStatus(space), refetchInterval: 60_000,
    queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/harbor-pilot/status', { params: { path: { space_id: space } }, signal })) })

/** The shared Git SSH keys a space's repositories may use. */
export const useSpaceGitKeys = (space: string) =>
  useQuery({ queryKey: keys.spaceGitKeys(space), queryFn: async ({ signal }) => unwrap(await api.GET('/spaces/{space_id}/git-ssh-keys', { params: { path: { space_id: space } }, signal })).items })
