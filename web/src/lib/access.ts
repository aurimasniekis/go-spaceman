import type { Me, Role } from '@/api/client'

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

/**
 * The highest role the principal holds in a domain (a space ID, or "*" for global grants, which cover every space).
 * This only decides what the UI offers; the API enforces every permission itself.
 */
export function roleIn(me: Me | null | undefined, domain: string): Role | null {
  if (!me) return null
  if (me.root) return 'admin'
  let best: Role | null = null
  for (const g of me.user?.grants ?? []) {
    if (g.domain !== domain && g.domain !== '*') continue
    const r = g.role as Role
    if (!best || rank[r] > rank[best]) best = r
  }
  return best
}

export function atLeast(role: Role | null, want: Role): boolean {
  return role !== null && rank[role] >= rank[want]
}

/** True when the principal holds any global grant (nodes, users and audit are global resources). */
export function hasGlobal(me: Me | null | undefined, want: Role = 'viewer'): boolean {
  return atLeast(roleIn(me, '*'), want)
}
