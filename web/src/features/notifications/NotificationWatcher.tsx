import { notifications } from '@mantine/notifications'
import { useEffect, useRef } from 'react'
import { api, unwrap } from '@/api/client'
import { useMe, useNotificationPrefs, useSpaces } from '@/api/queries'
import { changeListeners, type Change } from '@/api/watch'
import { closeButtonProps } from '@/lib/notify'
import { announcements } from './notify'

/**
 * Announces human-action changes to the signed-in user while the UI is open, as their notification preferences say
 * (SM-HUI-006): an in-app notification, and a desktop notification when allowed. It reads the changed action once per
 * change-feed entry; it never polls.
 */
export function NotificationWatcher() {
  const me = useMe().data
  const userId = me?.user?.id ?? ''
  const prefs = useNotificationPrefs(!!userId && !me?.limited).data
  const spaces = useSpaces().data
  const since = useRef(Date.now())
  const seen = useRef(new Set<string>())
  const state = useRef({ prefs, spaces, userId })
  state.current = { prefs, spaces, userId }
  useEffect(() => {
    const listener = (c: Change) => {
      const { prefs: p, spaces: sp, userId: uid } = state.current
      if (c.resource_type !== 'human_action' || !c.space_id || !p?.enabled || !uid) return
      if ((p.muted_spaces ?? []).includes(c.space_id)) return
      void api.GET('/spaces/{space_id}/human-actions/{action_id}', { params: { path: { space_id: c.space_id, action_id: c.resource_id } } })
        .then(unwrap)
        .then((a) => {
          const space = sp?.find((s) => s.id === a.space_id)?.name ?? a.space_id
          for (const n of announcements(a, { prefs: p, me: uid, created: c.action === 'created', since: since.current, seen: seen.current, space })) {
            seen.current.add(n.key)
            notifications.show({ color: 'owlBlue', title: n.title, message: n.body, closeButtonProps })
            if (p.desktop && typeof window !== 'undefined' && 'Notification' in window && Notification.permission === 'granted') {
              try {
                new Notification(n.title, { body: n.body, tag: n.key })
              } catch {
                // some browsers only allow notifications from a service worker; the in-app one was shown
              }
            }
          }
        })
        .catch(() => { /* an action this user cannot read, or one that is gone: nothing to announce */ })
    }
    changeListeners.add(listener)
    return () => { changeListeners.delete(listener) }
  }, [])
  return null
}
