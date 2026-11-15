import { useEffect, useRef } from 'react'
import { useRouter, useRouterState } from '@tanstack/react-router'
import { webApp } from './webapp'

/**
 * Telegram's BackButton follows the router: shown on every page but the first, it goes back in the history. Outside
 * Telegram (no WebApp) it does nothing.
 */
export function useBackButton() {
  const router = useRouter()
  const atRoot = useRouterState({ select: (s) => s.location.pathname === '/' })
  useEffect(() => {
    const b = webApp()?.BackButton
    if (!b) return
    const back = () => {
      // A deep link opens on a page with no history under it: go to the start page instead of leaving the app.
      if (router.history.canGoBack()) router.history.back()
      else void router.navigate({ to: '/' })
    }
    if (atRoot) b.hide()
    else b.show()
    b.onClick(back)
    return () => {
      b.offClick(back)
      b.hide()
    }
  }, [router, atRoot])
}

export interface MainButtonSpec {
  text: string
  onClick: () => void
  disabled?: boolean
  loading?: boolean
}

/**
 * Shows Telegram's MainButton while a page asks for it (an inline form's submit, say): its text, enabled state and
 * progress follow the spec; it is hidden again when the page goes. One page at a time owns it.
 */
export function useMainButton(spec: MainButtonSpec | null) {
  const click = useRef<() => void>(() => undefined)
  useEffect(() => {
    click.current = spec?.onClick ?? (() => undefined)
  }, [spec?.onClick])
  const on = spec !== null
  const { text, disabled, loading } = spec ?? { text: '', disabled: false, loading: false }
  useEffect(() => {
    const b = webApp()?.MainButton
    if (!b || !on) return
    const handler = () => click.current()
    b.onClick(handler)
    return () => {
      b.offClick(handler)
      b.hide()
    }
  }, [on])
  useEffect(() => {
    const b = webApp()?.MainButton
    if (!b || !on) return
    b.setParams({ text, is_active: !disabled && !loading, is_visible: true })
    if (loading) b.showProgress(false)
    else b.hideProgress()
  }, [on, text, disabled, loading])
}
