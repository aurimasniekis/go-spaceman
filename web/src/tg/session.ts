import { useSyncExternalStore } from 'react'
import { api } from '@/api/client'
import { clearTelegramSession, setMiniAppMode, setTelegramSession } from '@/api/session'
import { webApp } from './webapp'

/**
 * The Mini App's session state (plan §3.4.2, §4.6). `ended` is final: initData is single-use and stale by the time a
 * retry could happen, so the only way forward is the user reopening the app. Nothing here retries.
 */
export type TgSession =
  | { status: 'exchanging' }
  | { status: 'ready' }
  | { status: 'ended'; reason: string }

let state: TgSession = { status: 'exchanging' }
const listeners = new Set<() => void>()
const set = (s: TgSession) => {
  state = s
  for (const l of listeners) l()
}

export function endSession(reason = 'Session ended. Close and reopen the app.') {
  clearTelegramSession()
  if (state.status !== 'ended') set({ status: 'ended', reason })
}

export function isSessionEnded(): boolean {
  return state.status === 'ended'
}

export function useTgSession(): TgSession {
  return useSyncExternalStore(
    (l) => {
      listeners.add(l)
      return () => listeners.delete(l)
    },
    () => state,
  )
}

let started: Promise<void> | undefined

/**
 * Exchanges the WebApp's initData for a session, once per page load (a second call, such as StrictMode's, returns the
 * first: a second exchange of the same initData would be refused as a replay). Every failure ends the session.
 */
export function startSession(): Promise<void> {
  started ??= (async () => {
    setMiniAppMode((reason) => endSession(reason))
    const initData = webApp()?.initData
    if (!initData) return endSession('Open this app from Telegram.')
    try {
      const res = await api.POST('/telegram/session', { body: { init_data: initData } })
      if (res.data) {
        setTelegramSession(res.data.token)
        set({ status: 'ready' })
        return
      }
      const detail = (res.error as { detail?: string } | undefined)?.detail
      endSession(res.response.status === 403 && detail ? detail : 'Session ended. Close and reopen the app.')
    } catch {
      endSession('Could not reach SpaceMan. Close and reopen the app.')
    }
  })()
  return started
}
