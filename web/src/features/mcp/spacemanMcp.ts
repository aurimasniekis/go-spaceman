import type { GrantInput, Me, Schemas } from '@/api/client'
import { atLeast, roleIn } from '@/lib/access'

/** SpaceMan MCP servers (ADR-0063): SpaceMan's own API as tools for a space's control and worker runs. */

export type SpacemanTool = Schemas['GatewaySpacemanTool']
export type Preset = 'read' | 'operate' | 'admin' | 'custom'
export type ToolClass = SpacemanTool['class']

/**
 * The default slug. "spaceman" itself is reserved (the gateway refuses it: spaceman__<tool> would read as a built-in
 * spaceman_* tool), so the server gets "spaceman-admin".
 */
export const DEFAULT_SLUG = 'spaceman-admin'
export const DEFAULT_NAME = 'SpaceMan'
/** The space role granted the server by default: control (operator decision Q4). */
export const DEFAULT_ROLE = 'control'

/** Slugs the gateway accepts: 1-31 lowercase letters, digits or dashes, starting with a letter, not "spaceman". */
export function slugError(slug: string): string | null {
  if (!/^[a-z][a-z0-9-]{0,30}$/.test(slug) || slug.includes('__')) return '1-31 lowercase letters, digits or dashes, starting with a letter'
  if (slug === 'spaceman') return '"spaceman" is reserved for the built-in tools'
  return null
}

export type ScopeId = 'read-space' | 'operate-space' | 'read-all' | 'operate-all' | 'admin-all' | 'custom'

export type Scope = {
  id: ScopeId
  label: string
  description: string
  /** The preset the scope suggests for the tools. */
  preset: Preset
  /** Needs a space (the granted one). */
  space: boolean
  danger?: boolean
}

/** The scope choices, broadest last but one; the default is "Administer everything (infra)" (operator decision). */
export const SCOPES: Scope[] = [
  { id: 'read-space', label: 'Read-only, this space', description: 'Viewer on the granted space: read tools only.', preset: 'read', space: true },
  { id: 'operate-space', label: 'Operate this space', description: 'Operator on the granted space: reads and writes there.', preset: 'operate', space: true },
  { id: 'read-all', label: 'Read-only, all spaces', description: 'Viewer on every space and global reads.', preset: 'read', space: false },
  { id: 'operate-all', label: 'Operate all spaces', description: 'Operator on every space.', preset: 'operate', space: false },
  {
    id: 'admin-all', label: 'Administer everything (infra)', preset: 'admin', space: false, danger: true,
    description: 'Admin on every space and global administration, with dangerous tools. Every dangerous call needs a person’s approval.',
  },
  { id: 'custom', label: 'Custom grants', description: 'Choose the token’s role grants yourself.', preset: 'operate', space: false },
]

export const DEFAULT_SCOPE: ScopeId = 'admin-all'

/** The token grants a scope stands for ([] for custom, or a space scope without a space). */
export function scopeGrants(id: ScopeId, spaceId: string): GrantInput[] {
  switch (id) {
    case 'read-space':
      return spaceId ? [{ role: 'viewer', domain: spaceId }] : []
    case 'operate-space':
      return spaceId ? [{ role: 'operator', domain: spaceId }] : []
    case 'read-all':
      return [{ role: 'viewer', domain: '*' }]
    case 'operate-all':
      return [{ role: 'operator', domain: '*' }]
    case 'admin-all':
      return [{ role: 'admin', domain: '*' }]
    case 'custom':
      return []
  }
}

/** Whether the signed-in principal's own grants cover every grant (the API refuses a token reaching beyond its owner). */
export function covers(me: Me | null | undefined, grants: GrantInput[]): boolean {
  return grants.every((g) => atLeast(roleIn(me, g.domain), g.role))
}

/** Why a scope can't be chosen, or null. */
export function scopeBlocked(me: Me | null | undefined, s: Scope, spaceId: string): string | null {
  if (s.id === 'custom') return null
  if (s.space && !spaceId) return 'Choose the space first'
  return covers(me, scopeGrants(s.id, spaceId)) ? null : 'Beyond your own grants'
}

/** The first scope the principal may choose, preferring the default. */
export function initialScope(me: Me | null | undefined, spaceId: string): ScopeId {
  const def = SCOPES.find((s) => s.id === DEFAULT_SCOPE)
  if (def && !scopeBlocked(me, def, spaceId)) return def.id
  return [...SCOPES].reverse().find((s) => s.id !== 'custom' && !scopeBlocked(me, s, spaceId))?.id ?? 'custom'
}

export const PRESETS: { value: Preset; label: string; description: string }[] = [
  { value: 'read', label: 'Read', description: 'Read tools' },
  { value: 'operate', label: 'Read + write', description: 'Read and write tools' },
  { value: 'admin', label: 'Admin', description: 'Read, write and dangerous tools' },
  { value: 'custom', label: 'Custom', description: 'Only the tools you pick' },
]

/** The tool classes a preset offers without naming them (spacemanmcp.Preset.Classes). */
export function presetClasses(p: Preset): ToolClass[] {
  switch (p) {
    case 'read':
      return ['read']
    case 'operate':
      return ['read', 'write']
    case 'admin':
      return ['read', 'write', 'dangerous']
    case 'custom':
      return []
  }
}

export type ToolGroup = { tag: string; tools: SpacemanTool[] }

/** Tools grouped by their first tag, groups and tools sorted by name. */
export function groupByTag(tools: SpacemanTool[]): ToolGroup[] {
  const by = new Map<string, SpacemanTool[]>()
  for (const t of tools) {
    const tag = t.tags?.[0] ?? 'Other'
    by.set(tag, [...(by.get(tag) ?? []), t])
  }
  return [...by.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([tag, ts]) => ({ tag, tools: ts.sort((a, b) => a.name.localeCompare(b.name)) }))
}

/** The tools runs would see: selected and reachable by the grants. */
export function effectiveTools(tools: SpacemanTool[]): SpacemanTool[] {
  return tools.filter((t) => t.selected && t.reachable)
}

export const classColor: Record<ToolClass, string> = { read: 'gray', write: 'owlBlue', dangerous: 'owlRed', 'sensitive-read': 'owlYellow' }

/** The create request's expiry: omitted means the token never expires (the default). */
export function expiryDays(never: boolean, days: number): number | undefined {
  return never ? undefined : Math.min(366, Math.max(1, Math.round(days)))
}

/** The grant list in the tools query's role@domain form. */
export function grantsParam(grants: GrantInput[]): string[] {
  return grants.map((g) => `${g.role}@${g.domain}`)
}
