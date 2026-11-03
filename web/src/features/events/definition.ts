import type { Definition } from '@/api/client'

/**
 * Client-side checks of an event definition written as JSON (SM-EVT-017). They catch typing mistakes before a round
 * trip; the server stays authoritative (template.Definition.Validate runs on preview, create and update), so these
 * mirror only the structural rules of internal/events/template/definition.go and never relax them.
 */

const VAR_NAME = /^[a-z][a-z0-9_]{0,31}$/
const VARS_IN = /\{\{\s*vars\.([a-z][a-z0-9_]{0,31})\s*\}\}/g
const OPS = new Set(['eq', 'ne', 'in', 'exists', 'prefix'])
const URGENCY = new Set(['', 'low', 'normal', 'high', 'urgent'])

/** A small, valid definition to start a template or a custom subscription from. */
export const starterDefinition: Definition = {
  subject: 'evt.ops.alert.{{vars.environment}}',
  variables: [{ name: 'environment', pattern: '[a-z0-9-]+', required: true, description: 'The environment token in the subject', example: 'production' }],
  filters: [],
  title: '{{data.name}} ({{vars.environment}})',
  body: '**{{data.name}}** fired at {{event.time}}.\n\n{{data.summary | default "(no summary)" | truncate 4000}}',
  instructions: 'An alert fired. Decide whether it needs work in this space.',
  correlation: 'alert:{{data.name}}',
  urgency: 'high',
}

export type ParsedDefinition = { def?: Definition; errors: string[] }

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === 'object' && v !== null && !Array.isArray(v)
const isString = (v: unknown): v is string => typeof v === 'string'

/** Parses and checks a definition. With errors, `def` is still set when the text was a JSON object. */
export function parseDefinition(text: string): ParsedDefinition {
  let raw: unknown
  try {
    raw = JSON.parse(text)
  } catch (e) {
    return { errors: [`Not valid JSON: ${e instanceof Error ? e.message : String(e)}`] }
  }
  if (!isObject(raw)) return { errors: ['The definition must be a JSON object'] }
  const errors: string[] = []
  const add = (m: string) => errors.push(m)

  if (!isString(raw.subject) || raw.subject.trim() === '') add('subject: is required')
  else if (!raw.subject.startsWith('evt.')) add('subject: must be a Pulse filter under evt.')
  for (const k of ['title', 'body', 'instructions', 'correlation', 'urgency'] as const) {
    if (raw[k] !== undefined && !isString(raw[k])) add(`${k}: must be a string`)
  }
  if (!isString(raw.title) || raw.title.trim() === '') add('title: is required')
  if (isString(raw.urgency) && !URGENCY.has(raw.urgency)) add('urgency: must be low, normal, high or urgent')

  const names = new Set<string>()
  if (raw.variables !== undefined && !Array.isArray(raw.variables)) add('variables: must be a list')
  for (const [i, v] of (Array.isArray(raw.variables) ? raw.variables : []).entries()) {
    if (!isObject(v) || !isString(v.name)) {
      add(`variables[${i}]: needs a name`)
      continue
    }
    if (!VAR_NAME.test(v.name)) add(`variables: "${v.name}" is not a valid name`)
    else if (names.has(v.name)) add(`variables: "${v.name}" is defined twice`)
    names.add(v.name)
    if (v.pattern !== undefined) {
      if (!isString(v.pattern)) add(`variables.${v.name}.pattern: must be a string`)
      else {
        try {
          new RegExp(`^(?:${v.pattern})$`)
        } catch {
          add(`variables.${v.name}.pattern: is not a valid regular expression`)
        }
      }
    }
  }
  const undefinedVars = (where: string, s: unknown) => {
    if (!isString(s)) return
    for (const m of s.matchAll(VARS_IN)) if (!names.has(m[1])) add(`${where}: variable "${m[1]}" is not defined`)
  }
  undefinedVars('subject', raw.subject)

  if (raw.filters !== undefined && !Array.isArray(raw.filters)) add('filters: must be a list')
  for (const [i, f] of (Array.isArray(raw.filters) ? raw.filters : []).entries()) {
    if (!isObject(f)) {
      add(`filters[${i}]: must be an object`)
      continue
    }
    if (!isString(f.path) || !/^(event|data)(\.|$)/.test(f.path)) add(`filters[${i}].path: must be event.… or data.…`)
    if (!isString(f.op) || !OPS.has(f.op)) add(`filters[${i}].op: must be eq, ne, in, exists or prefix`)
    if (f.op === 'in' && (!Array.isArray(f.values) || f.values.length === 0)) add(`filters[${i}]: in needs values`)
    undefinedVars(`filters[${i}]`, f.value)
    for (const v of Array.isArray(f.values) ? f.values : []) undefinedVars(`filters[${i}]`, v)
  }
  if (isString(raw.instructions)) {
    const all = (raw.instructions.match(/\{\{/g) ?? []).length
    const vars = [...raw.instructions.matchAll(VARS_IN)].length
    if (all !== vars) add('instructions: may only use {{vars.name}} placeholders; event content is untrusted and cannot become instructions')
    undefinedVars('instructions', raw.instructions)
  }

  const def = { ...raw, variables: Array.isArray(raw.variables) ? raw.variables : [], filters: Array.isArray(raw.filters) ? raw.filters : [], body: isString(raw.body) ? raw.body : '' } as Definition
  return { def, errors }
}

/** A TanStack Form validator for a definition's JSON text: the first problem, or undefined. */
export const definitionError = ({ value }: { value: string }) => {
  const { errors } = parseDefinition(value)
  return errors.length === 0 ? undefined : errors.length === 1 ? errors[0] : `${errors[0]} (and ${errors.length - 1} more)`
}

/** Pretty JSON of a definition for the editor. */
export const definitionText = (d: Definition) => JSON.stringify(d, null, 2)
