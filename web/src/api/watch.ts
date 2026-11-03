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

/** Invalidates the cached queries a change affects (SM-UI-006: pages update live, never by polling the resources). */
export function applyChange(qc: QueryClient, c: Change) {
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
    default:
      break
  }
}

export type WatchState = 'connecting' | 'live' | 'offline'

/**
 * Subscribes to the durable change feed (GET /api/v1/watch, Server-Sent Events). EventSource reconnects by itself and
 * resumes with Last-Event-ID; a "gap" (the position was pruned) refetches everything. When the server ends the stream
 * because its session is no longer valid (SM-AUTH-007), the session is re-checked: a session that is gone goes to
 * sign-in, a rotated one reconnects.
 */
export function useWatch(enabled: boolean): WatchState {
  const qc = useQueryClient()
  const [state, setState] = useState<WatchState>('connecting')
  const [attempt, setAttempt] = useState(0)
  // The last change seen, so a reconnect after a session check resumes where the stream ended.
  const last = useRef(0)
  useEffect(() => {
    if (!enabled || typeof EventSource === 'undefined') return
    let closed = false
    const es = new EventSource(`${API_PREFIX}/watch${last.current > 0 ? `?after=${last.current}` : ''}`)
    es.onopen = () => setState('live')
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? 'offline' : 'connecting')
    es.addEventListener('change', (ev) => {
      last.current = Number((ev as MessageEvent<string>).lastEventId) || last.current
      try {
        applyChange(qc, JSON.parse((ev as MessageEvent<string>).data) as Change)
      } catch {
        // A malformed event is ignored; the next list refetch is authoritative.
      }
    })
    es.addEventListener('gap', () => void qc.invalidateQueries())
    es.addEventListener('end', (ev) => {
      es.close()
      if (endReason(ev) !== 'unauthenticated') {
        setState('offline')
        return
      }
      setState('connecting')
      void sessionStillValid().then((ok) => {
        if (closed) return
        if (ok) setAttempt((n) => n + 1)
        else setState('offline')
      })
    })
    return () => {
      closed = true
      es.close()
    }
  }, [enabled, qc, attempt])
  return state
}
