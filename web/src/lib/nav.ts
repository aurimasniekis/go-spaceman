import { IconActivity, IconAdjustments, IconBell, IconBox, IconLayoutDashboard, IconPlugConnected, IconShieldLock, IconTerminal2 } from '@tabler/icons-react'
import type { Me } from '@/api/client'
import { hasGlobal } from './access'

type Path = '/' | '/attention' | '/human' | '/terminals' | '/spaces' | '/nodes' | '/events' | '/accounts' | '/git-keys' | '/harbor-pilot' | '/prompts' | '/roles' | '/human-types' | '/approval-policies' | '/cross-space' | '/skills' | '/mcp-servers' | '/users' | '/sign-in-providers' | '/tokens' | '/audit' | '/backups' | '/system' | '/account'

interface Visibility {
  /** Shown only to global administrators (accounts hold credentials). */
  admin?: boolean
  /** Shown only with a global grant (the API decides; this only hides links that would 403). */
  global?: boolean
}

export interface NavItem extends Visibility {
  to: Path
  label: string
  icon: typeof IconBox
  /** Shows the number of pending human actions. */
  pendingBadge?: boolean
}

/** A page that belongs to a hub: listed as a tab on every member page and in the hub's sidebar entry. */
export interface HubTab extends Visibility {
  to: Path
  label: string
}

export type HubKey = 'configuration' | 'integrations' | 'access'

export interface Hub {
  key: HubKey
  /** The hub's own entry route; it redirects to the first tab the user may see. */
  to: '/configuration' | '/integrations' | '/access'
  label: string
  icon: typeof IconBox
  tabs: HubTab[]
}

/** The sidebar's plain entries. Attention is off the sidebar but its route stays (Overview lists the top items). */
export const nav: NavItem[] = [
  { to: '/', label: 'Overview', icon: IconLayoutDashboard },
  { to: '/human', label: 'Needs you', icon: IconBell, pendingBadge: true },
  { to: '/terminals', label: 'Terminals', icon: IconTerminal2 },
  { to: '/spaces', label: 'Spaces', icon: IconBox },
]

/** Every existing page keeps its URL; a hub only groups them. Account stays in the header's user menu. */
export const hubs: Hub[] = [
  {
    key: 'configuration', to: '/configuration', label: 'Configuration', icon: IconAdjustments,
    tabs: [
      { to: '/prompts', label: 'Prompts', global: true },
      { to: '/roles', label: 'Roles & profiles', global: true },
      { to: '/skills', label: 'Skills', global: true },
      { to: '/approval-policies', label: 'Approval policies', global: true },
      { to: '/human-types', label: 'Action types', global: true },
      { to: '/cross-space', label: 'Cross-space', global: true },
      { to: '/nodes', label: 'Nodes', global: true },
    ],
  },
  {
    key: 'integrations', to: '/integrations', label: 'Integrations', icon: IconPlugConnected,
    tabs: [
      { to: '/events', label: 'Events' },
      { to: '/mcp-servers', label: 'MCP servers', global: true },
      { to: '/accounts', label: 'Accounts', global: true, admin: true },
      { to: '/git-keys', label: 'Git SSH keys', global: true, admin: true },
      { to: '/harbor-pilot', label: 'Harbor Pilot', global: true, admin: true },
    ],
  },
  {
    key: 'access', to: '/access', label: 'Access', icon: IconShieldLock,
    tabs: [
      { to: '/users', label: 'Users', global: true },
      { to: '/sign-in-providers', label: 'Sign-in providers', global: true, admin: true },
      { to: '/tokens', label: 'API tokens' },
      { to: '/audit', label: 'Audit log', global: true },
      { to: '/backups', label: 'Backups', global: true, admin: true },
      { to: '/system', label: 'System', global: true, admin: true },
    ],
  },
]

export const activityIcon = IconActivity

const shown = (v: Visibility, me: Me | null | undefined) => (!v.global || hasGlobal(me)) && (!v.admin || hasGlobal(me, 'admin'))

export function visibleNav(me: Me | null | undefined): NavItem[] {
  return nav.filter((n) => shown(n, me))
}

/** The tabs of a hub the user may see. */
export function visibleTabs(hub: Hub, me: Me | null | undefined): HubTab[] {
  return hub.tabs.filter((t) => shown(t, me))
}

/** The hubs with at least one visible tab, each with only its visible tabs. */
export function visibleHubs(me: Me | null | undefined): Hub[] {
  return hubs.map((h) => ({ ...h, tabs: visibleTabs(h, me) })).filter((h) => h.tabs.length > 0)
}

export function hubOf(key: HubKey): Hub {
  return hubs.find((h) => h.key === key) as Hub
}

/** True when `path` is the page `to` or one of its detail pages. */
export function onPath(path: string, to: string): boolean {
  return to === '/' ? path === '/' : path === to || path.startsWith(`${to}/`)
}
