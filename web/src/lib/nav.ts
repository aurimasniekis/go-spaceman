import { IconActivity, IconBell, IconBolt, IconBox, IconKey, IconLayoutDashboard, IconListDetails, IconServer2, IconUserCircle, IconUsers } from '@tabler/icons-react'
import type { Me } from '@/api/client'
import { hasGlobal } from './access'

export interface NavItem {
  to: '/' | '/human' | '/spaces' | '/nodes' | '/events' | '/users' | '/tokens' | '/audit' | '/account'
  label: string
  icon: typeof IconBox
  group: 'Work' | 'Fleet' | 'Integrations' | 'Access'
  /** Shows the number of pending human actions. */
  pendingBadge?: boolean
  /** Shown only with a global grant (the API decides; this only hides links that would 403). */
  global?: boolean
}

export const nav: NavItem[] = [
  { to: '/', label: 'Overview', icon: IconLayoutDashboard, group: 'Work' },
  { to: '/human', label: 'Needs you', icon: IconBell, group: 'Work', pendingBadge: true },
  { to: '/spaces', label: 'Spaces', icon: IconBox, group: 'Work' },
  { to: '/nodes', label: 'Nodes', icon: IconServer2, group: 'Fleet', global: true },
  { to: '/events', label: 'Events', icon: IconBolt, group: 'Integrations' },
  { to: '/users', label: 'Users', icon: IconUsers, group: 'Access', global: true },
  { to: '/tokens', label: 'API tokens', icon: IconKey, group: 'Access' },
  { to: '/audit', label: 'Audit log', icon: IconListDetails, group: 'Access', global: true },
  { to: '/account', label: 'Account', icon: IconUserCircle, group: 'Access' },
]

export const activityIcon = IconActivity

export function visibleNav(me: Me | null | undefined): NavItem[] {
  return nav.filter((n) => !n.global || hasGlobal(me))
}
