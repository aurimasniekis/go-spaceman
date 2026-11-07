/** A profile's Claude Code settings: any settings.json keys (ADR-0059). */
export type ClaudeSettings = Record<string, unknown>

/** The entry that keeps Claude Code's built-in rules in an auto mode list. */
export const DEFAULTS = '$defaults'

export const PERMISSION_MODES = ['plan', 'default', 'acceptEdits', 'auto', 'dontAsk', 'bypassPermissions'] as const

/** An auto mode list as the form edits it: the rules, and whether they replace Claude Code's defaults. */
export interface AutoList {
  rules: string[]
  replace: boolean
}

/**
 * The profile form's view of Claude Code settings. Permissions, auto mode and env have their own editors; every other
 * key (hooks, statusLine, model, …) is JSON in `other`. `$defaults` is a switch, never typed.
 */
export interface ClaudeSettingsForm {
  defaultMode: string
  permAllow: string[]
  permAsk: string[]
  permDeny: string[]
  directories: string[]
  environment: AutoList
  allow: AutoList
  softDeny: AutoList
  env: [string, string][]
  other: string
}

const PERM_KEYS = ['defaultMode', 'allow', 'ask', 'deny', 'additionalDirectories']
const AUTO_KEYS = ['environment', 'allow', 'soft_deny']

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const strings = (v: unknown) => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [])
const without = (o: Record<string, unknown>, keys: string[]) => Object.fromEntries(Object.entries(o).filter(([k]) => !keys.includes(k)))
const autoFrom = (v: unknown): AutoList => {
  const all = strings(v)
  const rules = all.filter((r) => r !== DEFAULTS)
  return { rules, replace: rules.length > 0 && !all.includes(DEFAULTS) }
}
const clean = (l: string[]) => [...new Set(l.map((r) => r.trim()).filter((r) => r !== '' && r !== DEFAULTS))]
const autoTo = (a: AutoList) => {
  const rules = clean(a.rules)
  return rules.length === 0 ? [] : a.replace ? rules : [DEFAULTS, ...rules]
}

export function claudeSettingsForm(s?: ClaudeSettings): ClaudeSettingsForm {
  const settings = s ?? {}
  const perms = isObject(settings.permissions) ? settings.permissions : {}
  const auto = isObject(settings.autoMode) ? settings.autoMode : {}
  const env = isObject(settings.env) ? settings.env : {}
  // What the editors don't cover stays in `other`, nested keys of permissions and autoMode included.
  const other: Record<string, unknown> = without(settings, ['permissions', 'autoMode', 'env'])
  const permRest = without(perms, PERM_KEYS)
  const autoRest = without(auto, AUTO_KEYS)
  if (Object.keys(permRest).length) other.permissions = permRest
  if (Object.keys(autoRest).length) other.autoMode = autoRest
  return {
    defaultMode: typeof perms.defaultMode === 'string' ? perms.defaultMode : '',
    permAllow: strings(perms.allow),
    permAsk: strings(perms.ask),
    permDeny: strings(perms.deny),
    directories: strings(perms.additionalDirectories),
    environment: autoFrom(auto.environment),
    allow: autoFrom(auto.allow),
    softDeny: autoFrom(auto.soft_deny),
    env: Object.entries(env).map(([k, v]) => [k, typeof v === 'string' ? v : JSON.stringify(v)]),
    other: Object.keys(other).length ? JSON.stringify(other, null, 2) : '',
  }
}

/** The API value for the form, or the reason it can't be saved. Empty lists and blank rules are left out. */
export function claudeSettingsValue(f: ClaudeSettingsForm): { value: ClaudeSettings } | { error: string } {
  let other: unknown = {}
  if (f.other.trim() !== '') {
    try {
      other = JSON.parse(f.other)
    } catch {
      return { error: 'Other settings must be a JSON object.' }
    }
    if (!isObject(other)) return { error: 'Other settings must be a JSON object.' }
  }
  const out: ClaudeSettings = { ...(other as Record<string, unknown>) }
  if ('env' in out) return { error: 'Set environment variables in their own section, not in other settings.' }
  const put = (key: string, editorKeys: string[], values: Record<string, unknown>) => {
    const rest = isObject(out[key]) ? (out[key] as Record<string, unknown>) : {}
    const clash = editorKeys.find((k) => k in rest)
    if (clash) return `Set ${key}.${clash} in its own section, not in other settings.`
    const set = Object.fromEntries(Object.entries(values).filter(([, v]) => (Array.isArray(v) ? v.length > 0 : v !== '')))
    const merged = { ...rest, ...set }
    if (Object.keys(merged).length) out[key] = merged
    else delete out[key]
    return ''
  }
  const e1 = put('permissions', PERM_KEYS, { defaultMode: f.defaultMode, allow: clean(f.permAllow), ask: clean(f.permAsk), deny: clean(f.permDeny),
    additionalDirectories: clean(f.directories) })
  if (e1) return { error: e1 }
  const e2 = put('autoMode', AUTO_KEYS, { environment: autoTo(f.environment), allow: autoTo(f.allow), soft_deny: autoTo(f.softDeny) })
  if (e2) return { error: e2 }
  const env = f.env.filter(([k]) => k.trim() !== '')
  if (env.length) out.env = Object.fromEntries(env.map(([k, v]) => [k.trim(), v]))
  return { value: out }
}

/** The settings' top-level keys, for the profile list. */
export function claudeSettingsKeys(s?: ClaudeSettings): string[] {
  return Object.keys(s ?? {}).sort()
}
