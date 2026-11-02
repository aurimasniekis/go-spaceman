import type { Me, Role } from '@/api/client'

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 }

/**
 * The highest role the principal holds in a domain (a space ID, or "*" for global grants, which cover every space). SSO
 * sessions also hold the user's SSO-mapped grants.
 * This only decides what the UI offers; the API enforces every permission itself.
 */
export function roleIn(me: Me | null | undefined, domain: string): Role | null {
  if (!me) return null
  if (me.root) return 'admin'
  let best: Role | null = null
  // Grants from identity provider group mappings apply only to SSO sessions (the API enforces the same).
  const grants = [...(me.user?.grants ?? []), ...(me.method === 'oidc' ? (me.user?.sso_grants ?? []) : [])]
  for (const g of grants) {
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
