import { ActionIcon, Button, Checkbox, Fieldset, NumberInput, Radio, Select, Stack, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { IconTrash } from '@tabler/icons-react'

// The complex part of the schema-driven form (SM-HTYPE-003, SM-TG-006): nested objects, arrays of objects, local `$ref`s,
// `oneOf`/`anyOf` lists of constants and the common limits. Only data from the schema is read; nothing is executed. A
// schema using anything else (patternProperties, allOf merging, remote refs, a union of shapes) stays on the JSON editor.
// The server validates every submission against the schema regardless.

export type Node = {
  type?: string | string[]
  title?: string
  description?: string
  enum?: unknown[]
  const?: unknown
  oneOf?: Node[]
  anyOf?: Node[]
  $ref?: string
  format?: string
  items?: Node
  properties?: Record<string, Node>
  required?: string[]
  minimum?: number
  maximum?: number
  minLength?: number
  maxLength?: number
  minItems?: number
  maxItems?: number
  default?: unknown
}
export type UIField = { label?: string; help?: string; widget?: string; placeholder?: string }
export type UIMeta = { order?: string[]; fields?: Record<string, UIField> }

const MAX_DEPTH = 4

/** The node a local `#/$defs/x` or `#/definitions/x` reference names (anything else is unsupported). */
export function deref(n: Node, root: Node, hops = 0): Node | undefined {
  if (typeof n !== 'object' || n === null) return undefined
  if (!n.$ref) return n
  if (hops > 8) return undefined
  const m = /^#\/(\$defs|definitions)\/([^/]+)$/.exec(n.$ref)
  if (!m) return undefined
  const defs = (root as Record<string, unknown>)[m[1]] as Record<string, Node> | undefined
  let name: string
  try {
    name = decodeURIComponent(m[2])
  } catch {
    return undefined
  }
  const target = defs?.[name]
  const { $ref: _ref, ...rest } = n
  return target ? deref({ ...target, ...rest }, root, hops + 1) : undefined
}

export const kindOf = (n: Node) => (Array.isArray(n.type) ? n.type.find((t) => t !== 'null') : n.type)

/** The labelled options of a node: its enum, or a oneOf/anyOf of constants (with their titles). */
export function optionsOf(n: Node): { value: string; label: string; raw: unknown }[] | undefined {
  if (n.enum) return n.enum.map((v) => ({ value: String(v), label: String(v), raw: v }))
  const alts = n.oneOf ?? n.anyOf
  if (alts && alts.length > 0 && alts.every((a) => a.const !== undefined)) return alts.map((a) => ({ value: String(a.const), label: a.title ?? String(a.const), raw: a.const }))
  return undefined
}

/** Whether the node can be rendered as fields. */
export function supported(n0: Node, root: Node, depth = 0): boolean {
  if (depth > MAX_DEPTH) return false
  const n = deref(n0, root)
  if (!n) return false
  if ((n.oneOf || n.anyOf) && !optionsOf(n)) return false
  const k = kindOf(n)
  if (optionsOf(n)) return true
  if (k === 'string' || k === 'number' || k === 'integer' || k === 'boolean') return true
  if (k === 'object') return Object.values(n.properties ?? {}).every((p) => supported(p, root, depth + 1))
  if (k === 'array') return !!n.items && supported(n.items, root, depth + 1)
  return false
}

const empty = (x: unknown) => x === '' || x === undefined || x === null || (Array.isArray(x) && x.length === 0)

/** The value without empty fields: an empty string, a cleared number, an empty list or object means "not given". */
export function compactValue(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(compactValue).filter((x) => !empty(x) && !(typeof x === 'object' && x !== null && Object.keys(x).length === 0))
  if (typeof v === 'object' && v !== null) {
    const o = Object.fromEntries(Object.entries(v as Record<string, unknown>).map(([k, x]) => [k, compactValue(x)])
      .filter(([, x]) => !empty(x) && !(typeof x === 'object' && x !== null && !Array.isArray(x) && Object.keys(x).length === 0)))
    return o
  }
  return v
}

type Common = { root: Node; label: string; description?: string; required?: boolean; ui?: UIMeta; depth?: number }

/** One value (any supported node) as a controlled input; objects and arrays of objects nest. */
export function SchemaValue({ node, root, value, onChange, label, description, required, depth = 0 }: Common & { node: Node; value: unknown; onChange: (v: unknown) => void }) {
  const n = deref(node, root) ?? {}
  const k = kindOf(n)
  const desc = description ?? n.description
  const opts = optionsOf(n)
  if (k === 'boolean') return <Checkbox label={label} description={desc} checked={value === true} onChange={(e) => onChange(e.currentTarget.checked)} />
  if (opts && k !== 'array') {
    // The selected option's own value (a number or boolean enum keeps its type).
    const set = (x: string | null) => onChange(x === null ? undefined : opts.find((o) => o.value === x)?.raw)
    // A required short list is radios; an optional one stays a clearable select.
    if (required && opts.length <= 3) {
      return (
        <Radio.Group label={label} description={desc} withAsterisk={required} value={value === undefined ? '' : String(value)} onChange={(x) => set(x)}>
          <Stack gap={4} mt={4}>{opts.map((o) => <Radio key={o.value} value={o.value} label={o.label} />)}</Stack>
        </Radio.Group>
      )
    }
    return <Select label={label} description={desc} withAsterisk={required} clearable={!required} data={opts} value={value === undefined ? null : String(value)} onChange={set} />
  }
  if (k === 'number' || k === 'integer') {
    return <NumberInput label={label} description={desc} withAsterisk={required} min={n.minimum} max={n.maximum} allowDecimal={k === 'number'} value={typeof value === 'number' ? value : ''}
      onChange={(x) => onChange(typeof x === 'number' ? x : undefined)} />
  }
  if (k === 'string') {
    const inputType = n.format === 'date' ? 'date' : n.format === 'date-time' ? 'datetime-local' : n.format === 'email' ? 'email' : n.format === 'uri' ? 'url' : 'text'
    if (n.maxLength !== undefined && n.maxLength > 120) {
      return <Textarea label={label} description={desc} withAsterisk={required} autosize minRows={2} maxRows={10} maxLength={n.maxLength} value={typeof value === 'string' ? value : ''} onChange={(e) => onChange(e.target.value)} />
    }
    return <TextInput type={inputType} label={label} description={desc} withAsterisk={required} minLength={n.minLength} maxLength={n.maxLength} value={typeof value === 'string' ? value : ''}
      onChange={(e) => onChange(e.target.value)} />
  }
  if (k === 'object') {
    const props = n.properties ?? {}
    const v = (typeof value === 'object' && value !== null ? value : {}) as Record<string, unknown>
    return (
      <Fieldset legend={label} p="sm" role="group" aria-label={label}>
        {desc && <Text size="xs" c="dimmed" mb={6}>{desc}</Text>}
        <Stack gap="xs">
          {Object.keys(props).map((key) => (
            <SchemaValue key={key} node={props[key]} root={root} depth={depth + 1} label={props[key].title ?? key} required={(n.required ?? []).includes(key)}
              value={v[key]} onChange={(x) => onChange({ ...v, [key]: x })} />
          ))}
          {Object.keys(props).length === 0 && <Text size="sm" c="dimmed">No fields.</Text>}
        </Stack>
      </Fieldset>
    )
  }
  if (k === 'array') {
    const items = n.items ? (deref(n.items, root) ?? {}) : {}
    const list = Array.isArray(value) ? value : []
    const ik = kindOf(items)
    const iopts = optionsOf(items)
    // Lists of text or numbers are tags; lists of objects (or nested lists) are repeated groups.
    if (iopts || ik === 'string' || ik === 'number' || ik === 'integer') {
      const num = ik === 'number' || ik === 'integer'
      return <TagsInput label={label} description={desc} withAsterisk={required} data={iopts?.map((o) => o.value)} value={list.map(String)} maxTags={n.maxItems}
        onChange={(x) => onChange(num ? x.map(Number).filter((y) => !Number.isNaN(y)) : x)} />
    }
    const max = n.maxItems ?? 50
    return (
      <Fieldset legend={label} p="sm" role="group" aria-label={label}>
        {desc && <Text size="xs" c="dimmed" mb={6}>{desc}</Text>}
        <Stack gap="xs">
          {list.map((item, i) => (
            <Stack key={i} gap={4}>
              <SchemaValue node={items} root={root} depth={depth + 1} label={`${items.title ?? label} ${i + 1}`} value={item}
                onChange={(x) => onChange(list.map((y, j) => (j === i ? x : y)))} />
              <ActionIcon variant="subtle" color="owlRed" aria-label={`Remove ${items.title ?? label} ${i + 1}`} style={{ alignSelf: 'flex-end' }}
                onClick={() => onChange(list.filter((_, j) => j !== i))}><IconTrash size={14} /></ActionIcon>
            </Stack>
          ))}
          <Button size="xs" variant="default" w="fit-content" disabled={list.length >= max} onClick={() => onChange([...list, ik === 'object' ? {} : ik === 'boolean' ? false : ik === 'array' ? [] : undefined])}>Add {items.title ?? 'item'}</Button>
        </Stack>
      </Fieldset>
    )
  }
  return <Text size="sm" c="dimmed">{label}: this field is edited as JSON.</Text>
}
