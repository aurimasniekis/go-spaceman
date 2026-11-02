import { IconActivity, IconBell, IconBolt, IconCreditCard, IconFileText, IconForms, IconSparkles, IconUsersGroup, IconBox, IconKey, IconLayoutDashboard, IconListDetails, IconServer2, IconUserCircle, IconUsers } from '@tabler/icons-react'
import type { Me } from '@/api/client'
import { hasGlobal } from './access'

export interface NavItem {
  to: '/' | '/human' | '/spaces' | '/nodes' | '/events' | '/accounts' | '/prompts' | '/roles' | '/human-types' | '/skills' | '/users' | '/tokens' | '/audit' | '/account'
  label: string
  icon: typeof IconBox
  group: 'Work' | 'Fleet' | 'Configuration' | 'Integrations' | 'Access'
  /** Shows the number of pending human actions. */
  pendingBadge?: boolean
  /** Shown only to global administrators (accounts hold credentials). */
  admin?: boolean
  /** Shown only with a global grant (the API decides; this only hides links that would 403). */
  global?: boolean
}

export const nav: NavItem[] = [
  { to: '/', label: 'Overview', icon: IconLayoutDashboard, group: 'Work' },
  { to: '/human', label: 'Needs you', icon: IconBell, group: 'Work', pendingBadge: true },
  { to: '/spaces', label: 'Spaces', icon: IconBox, group: 'Work' },
  { to: '/nodes', label: 'Nodes', icon: IconServer2, group: 'Fleet', global: true },
  { to: '/prompts', label: 'Prompts', icon: IconFileText, group: 'Configuration', global: true },
  { to: '/roles', label: 'Roles & profiles', icon: IconUsersGroup, group: 'Configuration', global: true },
  { to: '/skills', label: 'Skills', icon: IconSparkles, group: 'Configuration', global: true },
  { to: '/human-types', label: 'Action types', icon: IconForms, group: 'Configuration', global: true },
  { to: '/events', label: 'Events', icon: IconBolt, group: 'Integrations' },
  { to: '/accounts', label: 'Accounts', icon: IconCreditCard, group: 'Integrations', global: true, admin: true },
  { to: '/users', label: 'Users', icon: IconUsers, group: 'Access', global: true },
  { to: '/tokens', label: 'API tokens', icon: IconKey, group: 'Access' },
  { to: '/audit', label: 'Audit log', icon: IconListDetails, group: 'Access', global: true },
  { to: '/account', label: 'Account', icon: IconUserCircle, group: 'Access' },
]

export const activityIcon = IconActivity

export function visibleNav(me: Me | null | undefined): NavItem[] {
  return nav.filter((n) => (!n.global || hasGlobal(me)) && (!n.admin || hasGlobal(me, 'admin')))
}
