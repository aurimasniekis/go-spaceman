import { claudeSettingsForm, claudeSettingsKeys, claudeSettingsValue, DEFAULTS } from './claudeSettings'

const settings = {
  permissions: { defaultMode: 'auto', allow: ['Bash(make test:*)'], deny: ['Read(./secrets/**)'], disableBypassPermissionsMode: 'disable' },
  autoMode: { environment: [DEFAULTS, 'Trusted hosts: example.com'], soft_deny: ['Only this'] },
  env: { FOO: 'bar' },
  hooks: { Stop: [{ hooks: [{ type: 'command', command: 'true' }] }] },
  model: 'opus',
}

describe('claude settings form', () => {
  it('splits the editors from other settings and puts everything back', () => {
    const f = claudeSettingsForm(settings)
    expect(f.defaultMode).toBe('auto')
    expect(f.environment).toEqual({ rules: ['Trusted hosts: example.com'], replace: false })
    expect(f.softDeny).toEqual({ rules: ['Only this'], replace: true })
    expect(f.env).toEqual([['FOO', 'bar']])
    expect(JSON.parse(f.other)).toEqual({ hooks: settings.hooks, model: 'opus', permissions: { disableBypassPermissionsMode: 'disable' } })
    expect(claudeSettingsValue(f)).toEqual({ value: settings })
    expect(claudeSettingsKeys(settings)).toEqual(['autoMode', 'env', 'hooks', 'model', 'permissions'])
  })

  it('leaves out empty parts and refuses JSON that is not an object or edits a key twice', () => {
    const f = claudeSettingsForm(undefined)
    expect(claudeSettingsValue(f)).toEqual({ value: {} })
    f.allow = { rules: ['  ', 'Pushing is allowed', 'Pushing is allowed'], replace: false }
    f.env = [['', 'ignored']]
    expect(claudeSettingsValue(f)).toEqual({ value: { autoMode: { allow: [DEFAULTS, 'Pushing is allowed'] } } })
    expect(claudeSettingsValue({ ...f, other: '[1]' })).toHaveProperty('error')
    expect(claudeSettingsValue({ ...f, other: '{' })).toHaveProperty('error')
    expect(claudeSettingsValue({ ...f, other: '{"autoMode": {"allow": []}}' })).toEqual({ error: 'Set autoMode.allow in its own section, not in other settings.' })
    expect(claudeSettingsValue({ ...f, other: '{"env": {}}' })).toHaveProperty('error')
  })
})
