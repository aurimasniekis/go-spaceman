import type { Me } from '@/api/client'
import { hubs, onPath, visibleHubs, visibleNav, visibleTabs } from './nav'

const me = (role: string | null, domain = '*'): Me =>
  ({ root: false, method: 'password', session_id: 's', limited: false, needs_enroll: false, expires_at: '', domains: [],
    user: { grants: role ? [{ role, domain, subject: 'user:u', source: 'manual' }] : [] } }) as unknown as Me

const tabs = (m: Me, key: string) => visibleHubs(m).find((h) => h.key === key)?.tabs.map((t) => t.to)

describe('settings hubs', () => {
  it('shows the admin every hub with every tab', () => {
    const m = me('admin')
    expect(visibleHubs(m).map((h) => h.label)).toEqual(['Configuration', 'Integrations', 'Access'])
    expect(tabs(m, 'configuration')).toEqual(['/prompts', '/roles', '/skills', '/approval-policies', '/human-types', '/cross-space', '/nodes'])
    expect(tabs(m, 'integrations')).toEqual(['/events', '/mcp-servers', '/accounts', '/git-keys', '/harbor-pilot'])
    expect(tabs(m, 'access')).toEqual(['/users', '/sign-in-providers', '/tokens', '/audit', '/backups', '/system'])
  })

  it.each(['viewer', 'operator'])('hides the admin-only tabs from a global %s', (role) => {
    const m = me(role)
    expect(tabs(m, 'configuration')).toHaveLength(7)
    expect(tabs(m, 'integrations')).toEqual(['/events', '/mcp-servers'])
    expect(tabs(m, 'access')).toEqual(['/users', '/tokens', '/audit'])
  })

  it('shows a user with space grants only Events and API tokens, and no Configuration', () => {
    for (const m of [me('admin', 'spc_1'), me('operator', 'spc_1'), me('viewer', 'spc_1'), me(null)]) {
      expect(visibleHubs(m).map((h) => h.key)).toEqual(['integrations', 'access'])
      expect(tabs(m, 'integrations')).toEqual(['/events'])
      expect(tabs(m, 'access')).toEqual(['/tokens'])
    }
  })

  it('shows nothing before the user is known except the pages without a global flag', () => {
    expect(visibleHubs(null).map((h) => h.key)).toEqual(['integrations', 'access'])
  })

  it('lists every existing page once, and keeps Account and Attention off the sidebar', () => {
    const all = hubs.flatMap((h) => h.tabs.map((t) => t.to))
    expect(new Set(all).size).toBe(all.length)
    expect(all).toHaveLength(18)
    expect([...all, ...visibleNav(me('admin')).map((n) => n.to)]).not.toEqual(expect.arrayContaining(['/account']))
    expect(visibleNav(me('admin')).map((n) => n.to)).toEqual(['/', '/human', '/terminals', '/spaces'])
  })

  it('takes the first visible tab as the entry target', () => {
    expect(visibleTabs(hubs[0], me('viewer'))[0].to).toBe('/prompts')
    expect(visibleTabs(hubs[1], me(null))[0].to).toBe('/events')
  })

  it('matches detail pages to their tab', () => {
    expect(onPath('/prompts/prm_1', '/prompts')).toBe(true)
    expect(onPath('/events/templates/t', '/events')).toBe(true)
    expect(onPath('/promptsx', '/prompts')).toBe(false)
  })
})
