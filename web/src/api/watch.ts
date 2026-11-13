import type { Query, QueryClient, QueryKey } from '@tanstack/react-query'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState } from 'react'
import { keys } from './queries'
import { API_PREFIX } from './session'
import { endReason, sessionStillValid } from './streams'

/** One change-feed entry (internal/changefeed.Change). */
export interface Change {
  seq: number
  at: number
  space_id?: string
  resource_type: string
  resource_id: string
  action: string
  version?: number
}

/** Resource types whose changes can change the attention summary (SM-UI-003). */
const ATTENTION = new Set(['space', 'run', 'node', 'task', 'human_action', 'event_subscription', 'space_limits', 'backup', 'relocation', 'restore'])

/** Listeners told of every change after the cache is invalidated (in-app notifications, SM-HUI-006). */
export const changeListeners = new Set<(c: Change) => void>()

/**
 * How long invalidations are collected before they are applied. A node reconnecting or a busy space can produce
 * hundreds of changes a second; each affected query is refetched once per window instead of once per change.
 */
export const INVALIDATE_WINDOW_MS = 250

const pending = new WeakMap<QueryClient, { keys: Map<string, QueryKey>; timer: ReturnType<typeof setTimeout> }>()

/** Queries with a fetch in flight that are invalidated again once it settles (refresh). */
const recheck = new WeakSet<Query>()

/**
 * Invalidates the queries matching queryKey (every query without one) without restarting a fetch in flight
 * (cancelRefetch: false): a stream of changes must never turn into a stream of requests. A fetch in flight may have
 * read its data before the change, though, and its success would mark that data fresh; so such a query is invalidated
 * once more when that fetch settles. That keeps at most one request per query in flight and loses no change.
 */
export function refresh(qc: QueryClient, queryKey?: QueryKey) {
  const inFlight = qc.getQueryCache().findAll({ ...(queryKey ? { queryKey } : {}), fetchStatus: 'fetching' })
  void qc.invalidateQueries(queryKey ? { queryKey } : undefined, { cancelRefetch: false })
  for (const q of inFlight) {
    const running = q.promise
    if (!running || recheck.has(q)) continue
    recheck.add(q)
    void running
      .catch(() => undefined)
      .then(() => {
        recheck.delete(q)
        return qc.invalidateQueries({ queryKey: q.queryKey, exact: true }, { cancelRefetch: false })
      })
  }
}

/**
 * Queues the invalidation of queryKey and applies the queued ones together at the end of the window, once per distinct
 * key (refresh).
 */
export function invalidate(qc: QueryClient, queryKey: QueryKey) {
  let p = pending.get(qc)
  if (!p) {
    const keys = new Map<string, QueryKey>()
    p = {
      keys,
      timer: setTimeout(() => {
        pending.delete(qc)
        for (const k of keys.values()) refresh(qc, k)
      }, INVALIDATE_WINDOW_MS),
    }
    pending.set(qc, p)
  }
  p.keys.set(JSON.stringify(queryKey), queryKey)
}

/** Invalidates the cached queries a change affects (SM-UI-006: pages update live, never by polling the resources). */
export function applyChange(qc: QueryClient, c: Change) {
  if (ATTENTION.has(c.resource_type)) invalidate(qc, ['attention'])
  switch (c.resource_type) {
    case 'space':
      invalidate(qc, keys.spaces)
      invalidate(qc, keys.queue(c.resource_id))
      invalidate(qc, keys.space(c.resource_id))
      invalidate(qc, keys.spaceAccounts(c.resource_id))
      invalidate(qc, keys.restore)
      invalidate(qc, keys.spaceRoles(c.resource_id))
      invalidate(qc, keys.spaceHumanTypes(c.resource_id))
      break
    case 'harbor_pilot_connection':
      invalidate(qc, keys.harborPilot)
      invalidate(qc, ['space-harbor-pilot'])
      break
    case 'space_harbor_pilot':
      invalidate(qc, keys.spaceHarborPilot(c.resource_id))
      invalidate(qc, keys.harborPilot)
      break
    case 'account':
      invalidate(qc, keys.accounts)
      invalidate(qc, ['space-accounts'])
      break
    case 'run':
      if (c.space_id) {
        invalidate(qc, keys.runs(c.space_id))
        invalidate(qc, keys.run(c.space_id, c.resource_id))
        invalidate(qc, ['usage', c.space_id])
      }
      break
    case 'node':
      invalidate(qc, keys.nodes)
      invalidate(qc, keys.node(c.resource_id))
      break
    case 'inbox_item':
      if (c.space_id) {
        invalidate(qc, keys.inbox(c.space_id))
        invalidate(qc, keys.inboxItem(c.space_id, c.resource_id))
      }
      break
    case 'event_subscription':
      if (c.space_id) {
        invalidate(qc, keys.subscriptions(c.space_id))
        invalidate(qc, keys.subscription(c.space_id, c.resource_id))
      }
      break
    case 'task':
      if (c.space_id) invalidate(qc, keys.queue(c.space_id))
      break
    case 'message':
      if (c.space_id) invalidate(qc, keys.messages(c.space_id))
      break
    case 'prompt':
      invalidate(qc, keys.prompts)
      invalidate(qc, keys.prompt(c.resource_id))
      break
    case 'profile':
      invalidate(qc, keys.profiles)
      break
    case 'dream':
      invalidate(qc, ['dreams'])
      break
    case 'kb':
      invalidate(qc, ['kbs'])
      break
    case 'memory':
      invalidate(qc, ['memory'])
      break
    case 'relocation':
      if (c.space_id) {
        invalidate(qc, keys.relocation(c.space_id))
        invalidate(qc, keys.space(c.space_id))
      }
      break
    case 'approval':
      invalidate(qc, ['approval'])
      break
    case 'approval_policy':
      invalidate(qc, keys.approvalPolicies)
      break
    case 'mcp_server':
      invalidate(qc, keys.mcpServers)
      invalidate(qc, ['mcp-grants'])
      invalidate(qc, ['space-mcp'])
      break
    case 'skill':
      invalidate(qc, keys.skills)
      break
    case 'human_type':
      invalidate(qc, keys.humanTypes)
      invalidate(qc, ['space-human-types'])
      break
    case 'role_template':
      invalidate(qc, keys.roleTemplates)
      break
    case 'human_policy':
      invalidate(qc, ['human-policies'])
      break
    case 'human_action':
      if (c.space_id) invalidate(qc, keys.human(c.space_id))
      invalidate(qc, keys.humanPending)
      invalidate(qc, ['human', 'search'])
      break
    case 'user':
      invalidate(qc, keys.users)
      break
    case 'token':
      invalidate(qc, ['tokens'])
      break
    case 'backup':
      invalidate(qc, keys.backups)
      break
    case 'restore':
      invalidate(qc, keys.restore)
      break
    case 'space_limits':
      invalidate(qc, keys.limits(c.resource_id))
      break
    case 'schedule':
      if (c.space_id) invalidate(qc, keys.schedules(c.space_id))
      break
    case 'space_repository':
    case 'space_repository_status':
      if (c.space_id) invalidate(qc, keys.repositories(c.space_id))
      break
    case 'xspace_request':
      invalidate(qc, ['xspace-requests'])
      invalidate(qc, keys.xspaceAll)
      break
    case 'xspace_peer':
      invalidate(qc, ['xspace-peers'])
      invalidate(qc, keys.xspaceAll)
      break
    case 'xspace_settings':
      invalidate(qc, ['xspace-settings'])
      break
    default:
      break
  }
}

export type WatchState = 'connecting' | 'live' | 'offline'

/** What the tab holding the change feed relays to the browser's other tabs. */
type Relay = { t: 'change'; c: Change; seq: number } | { t: 'gap' } | { t: 'state'; s: WatchState } | { t: 'hello' }

/**
 * The Web Lock and BroadcastChannel the tabs share. Versioned: a tab still running an older build (rc.8 and before)
 * could hold the lock with a dead stream and never retry, so every tab of the new build would only mirror its
 * "offline". A new name keeps old tabs out of the new tabs' election; bump it whenever the relay protocol or the
 * leader's duties change incompatibly.
 */
const CHANNEL = 'spaceman-watch.v2'

/** Reconnect backoff after the stream closed for good: 1 s, 2 s, 4 s … ±20 % jitter, never more than 30 s. */
export const RETRY_BASE_MS = 1000
export const RETRY_MAX_MS = 30_000
export function retryDelay(attempt: number, rand: number = Math.random()): number {
  return Math.min(RETRY_MAX_MS, Math.round(Math.min(RETRY_MAX_MS, RETRY_BASE_MS * 2 ** attempt) * (0.8 + 0.4 * rand)))
}

/**
 * Subscribes to the durable change feed (GET /api/v1/watch, Server-Sent Events). A tab without a position starts live
 * (?live=1: no replay of the retained history), learns its position from the stream's first event and refreshes its
 * active queries once. EventSource reconnects by itself and resumes with Last-Event-ID; a "gap" (the position was
 * pruned) refetches everything. Changes invalidate the queries they affect through invalidate (batched). When the server ends the stream
 * because its session is no longer valid (SM-AUTH-007), the session is re-checked: a session that is gone goes to
 * sign-in, a rotated one reconnects (at once the first time; repeated quick ends back off like a closed stream).
 *
 * EventSource gives up for good on a response that isn't a 200 text/event-stream (a 502 while central restarts, a
 * 401). Such a closed stream, and one the server ended for any other reason, is retried with backoff (retryDelay, reset
 * once a stream opens): each retry first re-checks the session (sessionStillValid), then reopens the stream from the
 * last change seen (?after=). It shows "offline" while waiting and "connecting" while trying. The window coming back
 * online, the leading tab becoming visible, and another tab saying hello retry at once.
 *
 * One stream serves every tab of the browser: over HTTP/1.1 a browser opens at most six connections to a host, for all
 * its tabs together, and each open stream holds one. The tab holding the Web Lock CHANNEL opens the stream and relays
 * it over a BroadcastChannel; when that tab closes, another one takes the lock and resumes from the last change it
 * saw. Without Web Locks (not a secure context) every tab opens its own stream.
 */
export function useWatch(enabled: boolean): WatchState {
  const qc = useQueryClient()
  const [state, setState] = useState<WatchState>('connecting')
  const current = useRef<WatchState>('connecting')
  // The last change seen (here or relayed), so a reconnect, or this tab taking over the stream, resumes from it.
  const last = useRef(0)
  useEffect(() => {
    if (!enabled || typeof EventSource === 'undefined') return
    const bc = typeof BroadcastChannel !== 'undefined' && typeof navigator !== 'undefined' && navigator.locks ? new BroadcastChannel(CHANNEL) : null
    let leading = !bc
    const show = (st: WatchState) => {
      current.current = st
      setState(st)
    }
    const publish = (st: WatchState) => {
      show(st)
      bc?.postMessage({ t: 'state', s: st } satisfies Relay)
    }
    const deliver = (c: Change, seq: number) => {
      if (seq > 0) last.current = Math.max(last.current, seq)
      applyChange(qc, c)
      for (const l of changeListeners) l(c)
    }

    // The stream itself, opened by the leading tab (or by every tab without Web Locks). Returns its cleanup and a
    // function that retries at once when the stream is down.
    const open = () => {
      let closed = false
      let es: EventSource | null = null // the stream that is open or opening; null while down
      let timer: ReturnType<typeof setTimeout> | undefined
      let checking = false // a session check before a retry is in flight
      let attempt = 0
      // Unauthenticated ends in a row of streams that were open less than RETRY_MAX_MS, and when the last stream
      // opened: a server that keeps ending fresh streams this way must not make the tab reconnect in a tight loop.
      let quickEnds = 0
      let openedAt = 0
      const clearTimer = () => {
        if (timer !== undefined) clearTimeout(timer)
        timer = undefined
      }
      // The stream is down: show offline and try again after the next backoff step.
      const later = () => {
        es = null
        publish('offline')
        clearTimer()
        timer = setTimeout(retry, retryDelay(attempt))
        attempt++
      }
      // Re-checks the session, then reopens the stream; a failed check waits for the next step. A no-op while a stream
      // is open or opening, or a check is in flight, so calling it at once (online, visible, hello) is always safe.
      const retry = () => {
        clearTimer()
        if (closed || checking || es) return
        checking = true
        publish('connecting')
        void sessionStillValid().then((ok) => {
          checking = false
          if (closed) return
          if (ok) connect()
          else later()
        })
      }
      const connect = () => {
        // Without a position yet (this tab has just loaded its data) the stream starts at the newest change instead of
        // replaying the retained history, which can be thousands of changes, each an invalidation.
        const src = new EventSource(`${API_PREFIX}/watch${last.current > 0 ? `?after=${last.current}` : '?live=1'}`)
        es = src
        src.onopen = () => {
          attempt = 0
          openedAt = Date.now()
          publish('live')
        }
        src.onerror = () => {
          if (es !== src) return
          if (src.readyState === EventSource.CLOSED) later()
          else publish('connecting')
        }
        src.addEventListener('change', (ev) => {
          const seq = Number((ev as MessageEvent<string>).lastEventId) || 0
          try {
            const c = JSON.parse((ev as MessageEvent<string>).data) as Change
            deliver(c, seq)
            bc?.postMessage({ t: 'change', c, seq } satisfies Relay)
          } catch {
            // A malformed event is ignored; the next list refetch is authoritative.
          }
        })
        // A live stream's first event: where the feed is now. Whatever changed between this tab loading its data and
        // that position is caught by refreshing the active queries once.
        src.addEventListener('position', (ev) => {
          const seq = Number((ev as MessageEvent<string>).lastEventId) || 0
          if (seq > 0) last.current = Math.max(last.current, seq)
          refresh(qc)
        })
        src.addEventListener('gap', () => {
          refresh(qc)
          bc?.postMessage({ t: 'gap' } satisfies Relay)
        })
        src.addEventListener('end', (ev) => {
          src.close()
          if (es !== src) return
          if (endReason(ev) === 'unauthenticated') {
            // Re-check the session: a rotated one reconnects, a gone one goes to sign-in. The first such end of a
            // stream that was open for a while does it at once; each further quick one waits for the next backoff step.
            es = null
            quickEnds = Date.now() - openedAt < RETRY_MAX_MS ? quickEnds + 1 : 1
            if (quickEnds === 1) retry()
            else {
              attempt = Math.max(attempt, quickEnds - 2)
              later()
            }
          } else later()
        })
      }
      // The window coming back online, or the leading tab becoming visible, retries at once (session check first).
      const now = () => retry()
      const onVisible = () => {
        if (document.visibilityState === 'visible') now()
      }
      window.addEventListener('online', now)
      document.addEventListener('visibilitychange', onVisible)
      connect()
      return {
        close: () => {
          closed = true
          clearTimer()
          window.removeEventListener('online', now)
          document.removeEventListener('visibilitychange', onVisible)
          es?.close()
          es = null
        },
        now,
      }
    }

    if (!bc) return open().close

    let stream: ReturnType<typeof open> | null = null

    bc.onmessage = (ev: MessageEvent<Relay>) => {
      const m = ev.data
      if (m.t === 'hello') {
        if (!leading) return
        bc.postMessage({ t: 'state', s: current.current } satisfies Relay)
        // A tab opened or reloaded: if this tab's stream is down (a throttled background tab waiting out its backoff),
        // try again now instead of pinning every tab to "offline".
        stream?.now()
        return
      }
      if (leading) return
      if (m.t === 'change') deliver(m.c, m.seq)
      else if (m.t === 'gap') refresh(qc)
      else show(m.s)
    }
    const stop = new AbortController()
    navigator.locks
      .request(CHANNEL, { signal: stop.signal }, () =>
        new Promise<void>((release) => {
          leading = true
          const st = open()
          stream = st
          stop.signal.addEventListener('abort', () => {
            st.close()
            stream = null
            leading = false
            release()
          }, { once: true })
        }),
      )
      .catch(() => {
        // Aborted while waiting for the lock: this tab was closed or signed out first.
      })
    bc.postMessage({ t: 'hello' } satisfies Relay) // a follower learns the stream's state from the leading tab
    return () => {
      stop.abort()
      bc.close()
    }
  }, [enabled, qc])
  return state
}
