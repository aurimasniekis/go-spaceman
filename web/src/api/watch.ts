import type { QueryClient } from '@tanstack/react-query'
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

/** Invalidates the cached queries a change affects (SM-UI-006: pages update live, never by polling the resources). */
export function applyChange(qc: QueryClient, c: Change) {
  if (ATTENTION.has(c.resource_type)) void qc.invalidateQueries({ queryKey: ['attention'] })
  switch (c.resource_type) {
    case 'space':
      void qc.invalidateQueries({ queryKey: keys.spaces })
      void qc.invalidateQueries({ queryKey: keys.queue(c.resource_id) })
      void qc.invalidateQueries({ queryKey: keys.space(c.resource_id) })
      void qc.invalidateQueries({ queryKey: keys.spaceAccounts(c.resource_id) })
      void qc.invalidateQueries({ queryKey: keys.restore })
      void qc.invalidateQueries({ queryKey: keys.spaceRoles(c.resource_id) })
      void qc.invalidateQueries({ queryKey: keys.spaceHumanTypes(c.resource_id) })
      break
    case 'account':
      void qc.invalidateQueries({ queryKey: keys.accounts })
      void qc.invalidateQueries({ queryKey: ['space-accounts'] })
      break
    case 'run':
      if (c.space_id) {
        void qc.invalidateQueries({ queryKey: keys.runs(c.space_id) })
        void qc.invalidateQueries({ queryKey: keys.run(c.space_id, c.resource_id) })
        void qc.invalidateQueries({ queryKey: ['usage', c.space_id] })
      }
      break
    case 'node':
      void qc.invalidateQueries({ queryKey: keys.nodes })
      void qc.invalidateQueries({ queryKey: keys.node(c.resource_id) })
      break
    case 'inbox_item':
      if (c.space_id) {
        void qc.invalidateQueries({ queryKey: keys.inbox(c.space_id) })
        void qc.invalidateQueries({ queryKey: keys.inboxItem(c.space_id, c.resource_id) })
      }
      break
    case 'event_subscription':
      if (c.space_id) {
        void qc.invalidateQueries({ queryKey: keys.subscriptions(c.space_id) })
        void qc.invalidateQueries({ queryKey: keys.subscription(c.space_id, c.resource_id) })
      }
      break
    case 'task':
      if (c.space_id) void qc.invalidateQueries({ queryKey: keys.queue(c.space_id) })
      break
    case 'message':
      if (c.space_id) void qc.invalidateQueries({ queryKey: keys.messages(c.space_id) })
      break
    case 'prompt':
      void qc.invalidateQueries({ queryKey: keys.prompts })
      void qc.invalidateQueries({ queryKey: keys.prompt(c.resource_id) })
      break
    case 'profile':
      void qc.invalidateQueries({ queryKey: keys.profiles })
      break
    case 'dream':
      void qc.invalidateQueries({ queryKey: ['dreams'] })
      break
    case 'kb':
      void qc.invalidateQueries({ queryKey: ['kbs'] })
      break
    case 'memory':
      void qc.invalidateQueries({ queryKey: ['memory'] })
      break
    case 'relocation':
      if (c.space_id) {
        void qc.invalidateQueries({ queryKey: keys.relocation(c.space_id) })
        void qc.invalidateQueries({ queryKey: keys.space(c.space_id) })
      }
      break
    case 'approval':
      void qc.invalidateQueries({ queryKey: ['approval'] })
      break
    case 'approval_policy':
      void qc.invalidateQueries({ queryKey: keys.approvalPolicies })
      break
    case 'mcp_server':
      void qc.invalidateQueries({ queryKey: keys.mcpServers })
      void qc.invalidateQueries({ queryKey: ['mcp-grants'] })
      void qc.invalidateQueries({ queryKey: ['space-mcp'] })
      break
    case 'skill':
      void qc.invalidateQueries({ queryKey: keys.skills })
      break
    case 'human_type':
      void qc.invalidateQueries({ queryKey: keys.humanTypes })
      void qc.invalidateQueries({ queryKey: ['space-human-types'] })
      break
    case 'role_template':
      void qc.invalidateQueries({ queryKey: keys.roleTemplates })
      break
    case 'human_policy':
      void qc.invalidateQueries({ queryKey: ['human-policies'] })
      break
    case 'human_action':
      if (c.space_id) void qc.invalidateQueries({ queryKey: keys.human(c.space_id) })
      void qc.invalidateQueries({ queryKey: keys.humanPending })
      void qc.invalidateQueries({ queryKey: ['human', 'search'] })
      break
    case 'user':
      void qc.invalidateQueries({ queryKey: keys.users })
      break
    case 'token':
      void qc.invalidateQueries({ queryKey: ['tokens'] })
      break
    case 'backup':
      void qc.invalidateQueries({ queryKey: keys.backups })
      break
    case 'restore':
      void qc.invalidateQueries({ queryKey: keys.restore })
      break
    case 'space_limits':
      void qc.invalidateQueries({ queryKey: keys.limits(c.resource_id) })
      break
    case 'schedule':
      if (c.space_id) void qc.invalidateQueries({ queryKey: keys.schedules(c.space_id) })
      break
    case 'space_repository':
    case 'space_repository_status':
      if (c.space_id) void qc.invalidateQueries({ queryKey: keys.repositories(c.space_id) })
      break
    case 'xspace_request':
      void qc.invalidateQueries({ queryKey: ['xspace-requests'] })
      void qc.invalidateQueries({ queryKey: keys.xspaceAll })
      break
    case 'xspace_peer':
      void qc.invalidateQueries({ queryKey: ['xspace-peers'] })
      void qc.invalidateQueries({ queryKey: keys.xspaceAll })
      break
    case 'xspace_settings':
      void qc.invalidateQueries({ queryKey: ['xspace-settings'] })
      break
    default:
      break
  }
}

export type WatchState = 'connecting' | 'live' | 'offline'

/** What the tab holding the change feed relays to the browser's other tabs. */
type Relay = { t: 'change'; c: Change; seq: number } | { t: 'gap' } | { t: 'state'; s: WatchState } | { t: 'hello' }

const CHANNEL = 'spaceman-watch'

/**
 * Subscribes to the durable change feed (GET /api/v1/watch, Server-Sent Events). EventSource reconnects by itself and
 * resumes with Last-Event-ID; a "gap" (the position was pruned) refetches everything. When the server ends the stream
 * because its session is no longer valid (SM-AUTH-007), the session is re-checked: a session that is gone goes to
 * sign-in, a rotated one reconnects.
 *
 * One stream serves every tab of the browser: over HTTP/1.1 a browser opens at most six connections to a host, for all
 * its tabs together, and each open stream holds one. The tab holding the Web Lock "spaceman-watch" opens the stream and
 * relays it over a BroadcastChannel; when that tab closes, another one takes the lock and resumes from the last change
 * it saw. Without Web Locks (not a secure context) every tab opens its own stream.
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

    // The stream itself, opened by the leading tab (or by every tab without Web Locks); returns its cleanup.
    const open = () => {
      let closed = false
      let es: EventSource | null = null
      const connect = () => {
        const src = new EventSource(`${API_PREFIX}/watch${last.current > 0 ? `?after=${last.current}` : ''}`)
        es = src
        src.onopen = () => publish('live')
        src.onerror = () => publish(src.readyState === EventSource.CLOSED ? 'offline' : 'connecting')
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
        src.addEventListener('gap', () => {
          void qc.invalidateQueries()
          bc?.postMessage({ t: 'gap' } satisfies Relay)
        })
        src.addEventListener('end', (ev) => {
          src.close()
          if (endReason(ev) !== 'unauthenticated') {
            publish('offline')
            return
          }
          publish('connecting')
          void sessionStillValid().then((ok) => {
            if (closed) return
            if (ok) connect()
            else publish('offline')
          })
        })
      }
      connect()
      return () => {
        closed = true
        es?.close()
      }
    }

    if (!bc) return open()

    bc.onmessage = (ev: MessageEvent<Relay>) => {
      const m = ev.data
      if (m.t === 'hello') {
        if (leading) bc.postMessage({ t: 'state', s: current.current } satisfies Relay)
        return
      }
      if (leading) return
      if (m.t === 'change') deliver(m.c, m.seq)
      else if (m.t === 'gap') void qc.invalidateQueries()
      else show(m.s)
    }
    const stop = new AbortController()
    navigator.locks
      .request(CHANNEL, { signal: stop.signal }, () =>
        new Promise<void>((release) => {
          leading = true
          const close = open()
          stop.signal.addEventListener('abort', () => {
            close()
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
