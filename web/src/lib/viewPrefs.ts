import { createStore, useSelector } from '@tanstack/react-store'
import { useCallback } from 'react'

/**
 * Per-browser view preferences (SM-UI-001): each list view's sort order and filters, kept in one TanStack Store so a
 * view keeps them while the user navigates, and remembered in localStorage between visits. They are conveniences only:
 * storage may be unavailable (private windows), and nothing here is sent to the server or shared.
 */
export type ViewState = Record<string, unknown>

const STORAGE_KEY = 'spaceman.views.v1'

function load(): Record<string, ViewState> {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    const v: unknown = raw ? JSON.parse(raw) : {}
    return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, ViewState>) : {}
  } catch {
    return {}
  }
}

export const viewPrefs = createStore<Record<string, ViewState>>(typeof window === 'undefined' ? {} : load())

viewPrefs.subscribe((v) => {
  try {
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(v))
  } catch {
    // storage unavailable or full: preferences last for this page only
  }
})

/** Sets (merges) part of a view's state. */
export function setView(view: string, patch: ViewState) {
  viewPrefs.setState((all) => ({ ...all, [view]: { ...all[view], ...patch } }))
}

/** Clears a view's state (back to its defaults). */
export function resetView(view: string) {
  viewPrefs.setState((all) => {
    const next = { ...all }
    delete next[view]
    return next
  })
}

/** A view's state merged over its defaults, and a setter. */
export function useView<T extends ViewState>(view: string, defaults: T): [T, (patch: Partial<T>) => void, () => void] {
  const stored = useSelector(viewPrefs, (all) => all[view])
  const state = { ...defaults, ...(stored as Partial<T> | undefined) } as T
  const set = useCallback((patch: Partial<T>) => setView(view, patch), [view])
  const reset = useCallback(() => resetView(view), [view])
  return [state, set, reset]
}
