import { Checkbox, Group, NumberInput, Radio, Select, Stack, Switch, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { useState } from 'react'

// A JSON Schema 2020-12 subset rendered as a form (SM-HTYPE-003). Only data from the schema and the type's safe UI
// metadata is used; nothing in either is executed. Fields the form cannot render, and anyone who prefers it, use the
// validated JSON editor. The server validates every submission against the schema regardless.

type Prop = {
  type?: string | string[]
  title?: string
  description?: string
  enum?: unknown[]
  format?: string
  items?: { type?: string; enum?: unknown[] }
  default?: unknown
}
type ObjectSchema = { type?: string; properties?: Record<string, Prop>; required?: string[] }
type UIField = { label?: string; help?: string; widget?: string; placeholder?: string }
type UIMeta = { order?: string[]; fields?: Record<string, UIField> }

const kind = (p: Prop) => (Array.isArray(p.type) ? p.type.find((t) => t !== 'null') : p.type)

/** Whether every property can be rendered as a field (otherwise the JSON editor is used). */
export function formable(schema: unknown): boolean {
  const s = (schema ?? {}) as ObjectSchema
  return Object.values(s.properties ?? {}).every((p) => {
    const k = kind(p)
    return k === 'string' || k === 'number' || k === 'integer' || k === 'boolean' || (k === 'array' && (p.items?.type === 'string' || !!p.items?.enum))
  })
}

export function SchemaForm({ schema, ui, value, onChange, label }: { schema: unknown; ui?: unknown; value: Record<string, unknown>; onChange: (v: Record<string, unknown>) => void; label: string }) {
  const s = (schema ?? {}) as ObjectSchema
  const meta = (ui ?? {}) as UIMeta
  const canForm = formable(schema)
  const [json, setJson] = useState(!canForm)
  const [text, setText] = useState(JSON.stringify(value, null, 2))
  const [parseError, setParseError] = useState('')
  const props = s.properties ?? {}
  const order = [...(meta.order ?? []).filter((k) => k in props), ...Object.keys(props).filter((k) => !(meta.order ?? []).includes(k))]
  const set = (k: string, v: unknown) => {
    const next = { ...value }
    if (v === '' || v === undefined || (Array.isArray(v) && v.length === 0)) delete next[k]
    else next[k] = v
    onChange(next)
  }
  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Text size="sm" fw={600}>{label}</Text>
        <Switch size="xs" label="Edit as JSON" checked={json} disabled={!canForm} onChange={(e) => {
          setJson(e.currentTarget.checked)
          setText(JSON.stringify(value, null, 2))
        }} />
      </Group>
      {json ? (
        <Textarea aria-label={`${label} (JSON)`} classNames={{ input: 'mono' }} autosize minRows={4} maxRows={16} value={text} error={parseError || undefined}
          description={canForm ? undefined : 'This type uses fields the form cannot show; edit the JSON directly.'}
          onChange={(e) => {
            setText(e.target.value)
            try {
              const v = JSON.parse(e.target.value || '{}') as unknown
              if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new Error('must be a JSON object')
              setParseError('')
              onChange(v as Record<string, unknown>)
            } catch (err) {
              setParseError(`Not valid: ${(err as Error).message}`)
            }
          }} />
      ) : order.length === 0 ? <Text size="sm" c="dimmed">No fields.</Text> : order.map((k) => {
        const p = props[k]
        const f = meta.fields?.[k] ?? {}
        const fieldLabel = f.label || p.title || k
        const desc = f.help || p.description
        const required = (s.required ?? []).includes(k)
        const t = kind(p)
        const v = value[k]
        if (t === 'boolean') return <Checkbox key={k} label={fieldLabel} description={desc} checked={v === true} onChange={(e) => set(k, e.currentTarget.checked)} />
        if (t === 'number' || t === 'integer') {
          return <NumberInput key={k} label={fieldLabel} description={desc} withAsterisk={required} allowDecimal={t === 'number'} value={typeof v === 'number' ? v : ''}
            onChange={(n) => set(k, typeof n === 'number' ? n : undefined)} />
        }
        if (t === 'array') {
          const opts = (p.items?.enum ?? []).map(String)
          return <TagsInput key={k} label={fieldLabel} description={desc} withAsterisk={required} data={opts} value={Array.isArray(v) ? v.map(String) : []}
            onChange={(x) => set(k, x)} />
        }
        if (p.enum) {
          const opts = p.enum.map(String)
          if (f.widget === 'radio') {
            return (
              <Radio.Group key={k} label={fieldLabel} description={desc} withAsterisk={required} value={typeof v === 'string' ? v : ''} onChange={(x) => set(k, x)}>
                <Stack gap={4} mt={4}>{opts.map((o) => <Radio key={o} value={o} label={o} />)}</Stack>
              </Radio.Group>
            )
          }
          return <Select key={k} label={fieldLabel} description={desc} withAsterisk={required} clearable={!required} data={opts} value={typeof v === 'string' ? v : null}
            onChange={(x) => set(k, x ?? undefined)} />
        }
        if (f.widget === 'textarea' || f.widget === 'markdown') {
          return <Textarea key={k} label={fieldLabel} description={desc} withAsterisk={required} placeholder={f.placeholder} autosize minRows={2} maxRows={10}
            value={typeof v === 'string' ? v : ''} onChange={(e) => set(k, e.target.value)} />
        }
        const inputType = p.format === 'date' || f.widget === 'date' ? 'date' : p.format === 'date-time' || f.widget === 'datetime' ? 'datetime-local'
          : p.format === 'email' || f.widget === 'email' ? 'email' : p.format === 'uri' || f.widget === 'url' ? 'url' : 'text'
        return <TextInput key={k} type={inputType} label={fieldLabel} description={desc} withAsterisk={required} placeholder={f.placeholder}
          value={typeof v === 'string' ? v : ''} onChange={(e) => set(k, e.target.value)} />
      })}
    </Stack>
  )
}

/** A read-only rendering of request or response data (plain text, never HTML). */
export function DataView({ data, schema, ui }: { data: unknown; schema?: unknown; ui?: unknown }) {
  const obj = (data ?? {}) as Record<string, unknown>
  const props = ((schema ?? {}) as ObjectSchema).properties ?? {}
  const meta = (ui ?? {}) as UIMeta
  const keys = Object.keys(obj)
  if (keys.length === 0) return <Text size="sm" c="dimmed">No data.</Text>
  return (
    <Stack gap={4}>
      {keys.map((k) => (
        <Group key={k} gap="xs" align="flex-start" wrap="nowrap">
          <Text size="sm" c="dimmed" w={160} style={{ flexShrink: 0 }}>{meta.fields?.[k]?.label || props[k]?.title || k}</Text>
          <Text size="sm" style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{typeof obj[k] === 'string' ? (obj[k] as string) : JSON.stringify(obj[k])}</Text>
        </Group>
      ))}
    </Stack>
  )
}
