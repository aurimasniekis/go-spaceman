import { describe, expect, it } from 'vitest'
import type { Me } from '@/api/client'
import {
  covers, DEFAULT_SCOPE, DEFAULT_SLUG, effectiveTools, expiryDays, grantsParam, groupByTag, initialScope, presetClasses, SCOPES, scopeBlocked, scopeGrants,
  slugError, type SpacemanTool,
} from './spacemanMcp'

const me = (grants: { role: string; domain: string }[], extra: Partial<Me> = {}) =>
  ({ root: false, method: 'password', limited: false, needs_enroll: false, domains: [], expires_at: '', session_id: 's', user: { id: 'usr_1', grants }, ...extra }) as unknown as Me

const admin = me([{ role: 'admin', domain: '*' }])
const spaceOperator = me([{ role: 'operator', domain: 'spc_a' }])
const globalViewer = me([{ role: 'viewer', domain: '*' }, { role: 'operator', domain: 'spc_a' }])
const scope = (id: string) => SCOPES.find((s) => s.id === id)!

describe('SpaceMan MCP presets and limits', () => {
  it('default to a slug other than the reserved "spaceman"', () => {
    expect(DEFAULT_SLUG).not.toBe('spaceman')
    expect(slugError(DEFAULT_SLUG)).toBeNull()
    expect(slugError('spaceman')).toMatch(/reserved/)
    expect(slugError('Bad Slug')).not.toBeNull()
    expect(slugError('a'.repeat(32))).not.toBeNull()
    expect(slugError('ops__x')).not.toBeNull()
  })

  it('default the scope to "Administer everything (infra)" with the admin preset', () => {
    expect(DEFAULT_SCOPE).toBe('admin-all')
    expect(scope('admin-all').label).toBe('Administer everything (infra)')
    expect(scope('admin-all').preset).toBe('admin')
    expect(initialScope(admin, '')).toBe('admin-all')
  })

  it('map each scope to its token grants', () => {
    expect(scopeGrants('read-space', 'spc_a')).toEqual([{ role: 'viewer', domain: 'spc_a' }])
    expect(scopeGrants('operate-space', 'spc_a')).toEqual([{ role: 'operator', domain: 'spc_a' }])
    expect(scopeGrants('read-space', '')).toEqual([])
    expect(scopeGrants('read-all', '')).toEqual([{ role: 'viewer', domain: '*' }])
    expect(scopeGrants('operate-all', '')).toEqual([{ role: 'operator', domain: '*' }])
    expect(scopeGrants('admin-all', '')).toEqual([{ role: 'admin', domain: '*' }])
    expect(scopeGrants('custom', 'spc_a')).toEqual([])
  })

  it('disable scopes beyond the caller’s own grants', () => {
    // An operator of one space: only that space's scopes.
    expect(scopeBlocked(spaceOperator, scope('admin-all'), 'spc_a')).toBe('Beyond your own grants')
    expect(scopeBlocked(spaceOperator, scope('operate-all'), 'spc_a')).toBe('Beyond your own grants')
    expect(scopeBlocked(spaceOperator, scope('read-all'), 'spc_a')).toBe('Beyond your own grants')
    expect(scopeBlocked(spaceOperator, scope('operate-space'), 'spc_a')).toBeNull()
    expect(scopeBlocked(spaceOperator, scope('operate-space'), 'spc_b')).toBe('Beyond your own grants')
    expect(scopeBlocked(spaceOperator, scope('read-space'), '')).toBe('Choose the space first')
    expect(initialScope(spaceOperator, 'spc_a')).toBe('operate-space')
    expect(initialScope(spaceOperator, '')).toBe('custom')
    // A global viewer who operates one space: global reads, not global operation.
    expect(scopeBlocked(globalViewer, scope('read-all'), '')).toBeNull()
    expect(scopeBlocked(globalViewer, scope('operate-all'), '')).toBe('Beyond your own grants')
    expect(initialScope(globalViewer, '')).toBe('read-all')
    // Custom grants are checked by covers (and by the API).
    expect(scopeBlocked(spaceOperator, scope('custom'), '')).toBeNull()
    expect(covers(spaceOperator, [{ role: 'viewer', domain: 'spc_a' }])).toBe(true)
    expect(covers(spaceOperator, [{ role: 'admin', domain: 'spc_a' }])).toBe(false)
    expect(covers(admin, [{ role: 'admin', domain: '*' }, { role: 'operator', domain: 'spc_z' }])).toBe(true)
    expect(covers(me([], { root: true }), [{ role: 'admin', domain: '*' }])).toBe(true)
  })

  it('offer the preset classes like the server', () => {
    expect(presetClasses('read')).toEqual(['read'])
    expect(presetClasses('operate')).toEqual(['read', 'write'])
    expect(presetClasses('admin')).toEqual(['read', 'write', 'dangerous'])
    expect(presetClasses('custom')).toEqual([])
  })

  it('group tools by tag and count the effective ones', () => {
    const t = (name: string, tag: string, selected: boolean, reachable: boolean): SpacemanTool =>
      ({ name, tags: [tag], class: 'read', summary: name, in_preset: selected, selected, reachable })
    const tools = [t('list_spaces', 'Spaces', true, true), t('list_nodes', 'Nodes', true, false), t('get_space', 'Spaces', false, true), t('create_task', 'Tasks', true, true)]
    expect(groupByTag(tools).map((g) => [g.tag, g.tools.map((x) => x.name)])).toEqual([
      ['Nodes', ['list_nodes']], ['Spaces', ['get_space', 'list_spaces']], ['Tasks', ['create_task']],
    ])
    expect(effectiveTools(tools).map((x) => x.name)).toEqual(['list_spaces', 'create_task'])
  })

  it('never expire by default and bound the days', () => {
    expect(expiryDays(true, 90)).toBeUndefined()
    expect(expiryDays(false, 90)).toBe(90)
    expect(expiryDays(false, 1000)).toBe(366)
    expect(expiryDays(false, 0)).toBe(1)
    expect(grantsParam([{ role: 'admin', domain: '*' }, { role: 'viewer', domain: 'spc_a' }])).toEqual(['admin@*', 'viewer@spc_a'])
  })
})
