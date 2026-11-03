import { Checkbox, Group, NumberInput, Radio, Select, Stack, Switch, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { useForm, useSelector } from '@tanstack/react-form'
import { fieldError, required } from '@/lib/form'

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

/** The value without empty fields: an empty string, a cleared number or an empty list means "not given". */
function compact(v: Record<string, unknown>): Record<string, unknown> {
  return Object.fromEntries(Object.entries(v).filter(([, x]) => !(x === '' || x === undefined || x === null || (Array.isArray(x) && x.length === 0))))
}

function parseObject(text: string): Record<string, unknown> {
  const v = JSON.parse(text || '{}') as unknown
  if (typeof v !== 'object' || v === null || Array.isArray(v)) throw new Error('must be a JSON object')
  return v as Record<string, unknown>
}

/**
 * The fields of a schema as a TanStack Form (SM-ATT-007): each property is a form field (required ones validated), and
 * the JSON fallback is a validated text field. Every change reports the compacted object through onChange, so a parent
 * form holds it as one field.
 */
export function SchemaForm({ schema, ui, value, onChange, label }: { schema: unknown; ui?: unknown; value: Record<string, unknown>; onChange: (v: Record<string, unknown>) => void; label: string }) {
  const s = (schema ?? {}) as ObjectSchema
  const meta = (ui ?? {}) as UIMeta
  const canForm = formable(schema)
  const form = useForm({ defaultValues: { data: value, json: !canForm, text: JSON.stringify(value, null, 2) } })
  const json = useSelector(form.store, (st) => st.values.json)
  const props = s.properties ?? {}
  const order = [...(meta.order ?? []).filter((k) => k in props), ...Object.keys(props).filter((k) => !(meta.order ?? []).includes(k))]
  const emit = () => onChange(compact(form.state.values.data))
  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Text size="sm" fw={600}>{label}</Text>
        <form.Field name="json">
          {(f) => (
            <Switch size="xs" label="Edit as JSON" checked={f.state.value} disabled={!canForm} onChange={(e) => {
              f.handleChange(e.currentTarget.checked)
              form.setFieldValue('text', JSON.stringify(compact(form.state.values.data), null, 2))
            }} />
          )}
        </form.Field>
      </Group>
      {json ? (
        <form.Field name="text" validators={{
          onChange: ({ value: t }) => {
            try {
              parseObject(t)
              return undefined
            } catch (err) {
              return `Not valid: ${(err as Error).message}`
            }
          },
        }}>
          {(f) => (
            <Textarea aria-label={`${label} (JSON)`} classNames={{ input: 'mono' }} autosize minRows={4} maxRows={16} value={f.state.value}
              error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
              description={canForm ? undefined : 'This type uses fields the form cannot show; edit the JSON directly.'}
              onBlur={f.handleBlur}
              onChange={(e) => {
                f.handleChange(e.target.value)
                try {
                  const v = parseObject(e.target.value)
                  form.setFieldValue('data', v)
                  onChange(v)
                } catch {
                  // the field's validator shows the error; the last valid object stays in force
                }
              }} />
          )}
        </form.Field>
      ) : order.length === 0 ? <Text size="sm" c="dimmed">No fields.</Text> : order.map((k) => {
        const p = props[k]
        const fm = meta.fields?.[k] ?? {}
        const fieldLabel = fm.label || p.title || k
        const desc = fm.help || p.description
        const req = (s.required ?? []).includes(k)
        const t = kind(p)
        const validators = req && t !== 'boolean' ? { onBlur: required(fieldLabel), onChange: required(fieldLabel) } : undefined
        return (
          <form.Field key={k} name={`data.${k}`} validators={validators}>
            {(f) => {
              const v = f.state.value
              const set = (x: unknown) => {
                f.handleChange(x)
                emit()
              }
              const error = fieldError(f.state.meta)
              if (t === 'boolean') return <Checkbox label={fieldLabel} description={desc} checked={v === true} onChange={(e) => set(e.currentTarget.checked)} />
              if (t === 'number' || t === 'integer') {
                return <NumberInput label={fieldLabel} description={desc} withAsterisk={req} allowDecimal={t === 'number'} value={typeof v === 'number' ? v : ''} error={error}
                  onBlur={f.handleBlur} onChange={(n) => set(typeof n === 'number' ? n : undefined)} />
              }
              if (t === 'array') {
                const opts = (p.items?.enum ?? []).map(String)
                return <TagsInput label={fieldLabel} description={desc} withAsterisk={req} data={opts} value={Array.isArray(v) ? v.map(String) : []} error={error}
                  onBlur={f.handleBlur} onChange={(x) => set(x)} />
              }
              if (p.enum) {
                const opts = p.enum.map(String)
                if (fm.widget === 'radio') {
                  return (
                    <Radio.Group label={fieldLabel} description={desc} withAsterisk={req} value={typeof v === 'string' ? v : ''} error={error} onChange={(x) => set(x)}>
                      <Stack gap={4} mt={4}>{opts.map((o) => <Radio key={o} value={o} label={o} />)}</Stack>
                    </Radio.Group>
                  )
                }
                return <Select label={fieldLabel} description={desc} withAsterisk={req} clearable={!req} data={opts} value={typeof v === 'string' ? v : null} error={error}
                  onBlur={f.handleBlur} onChange={(x) => set(x ?? undefined)} />
              }
              if (fm.widget === 'textarea' || fm.widget === 'markdown') {
                return <Textarea label={fieldLabel} description={desc} withAsterisk={req} placeholder={fm.placeholder} autosize minRows={2} maxRows={10} error={error}
                  value={typeof v === 'string' ? v : ''} onBlur={f.handleBlur} onChange={(e) => set(e.target.value)} />
              }
              const inputType = p.format === 'date' || fm.widget === 'date' ? 'date' : p.format === 'date-time' || fm.widget === 'datetime' ? 'datetime-local'
                : p.format === 'email' || fm.widget === 'email' ? 'email' : p.format === 'uri' || fm.widget === 'url' ? 'url' : 'text'
              return <TextInput type={inputType} label={fieldLabel} description={desc} withAsterisk={req} placeholder={fm.placeholder} error={error}
                value={typeof v === 'string' ? v : ''} onBlur={f.handleBlur} onChange={(e) => set(e.target.value)} />
            }}
          </form.Field>
        )
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
