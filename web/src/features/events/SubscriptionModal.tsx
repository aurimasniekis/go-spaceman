import { Accordion, Alert, Button, Group, JsonInput, Modal, SegmentedControl, Select, SimpleGrid, Stack, Switch, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Cursor, type Definition, type Subscription } from '@/api/client'
import { keys, useConnections, useTemplates } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { coreExplanation, whilePausedOptions } from './badges'
import { definitionError, definitionText, parseDefinition, starterDefinition } from './definition'
import { PreviewPanel } from './PreviewPanel'
import { placeholderHelp } from './TemplateEditor'

type Variable = Definition['variables'][number]

const varError = (v: Variable) => ({ value }: { value: string | undefined }) =>
  v.required && !value ? `${v.name} is required` : v.pattern && value && !new RegExp(`^(?:${v.pattern})$`).test(value) ? `Must match ${v.pattern}` : undefined

const FORM_ID = 'subscription-form'

/**
 * Subscribes a space to a published template version or to a custom definition (SM-EVT-005, SM-EVT-017). Variable
 * fields come from the template or the custom definition; the server binds and validates everything before the
 * subscription starts. A custom definition is checked as it is typed (structure only) and can be tested against a sample
 * event through the preview API, which runs the server's validation. By default only new events are delivered.
 * Delivery is a JetStream durable or, for sources that publish without JetStream, a non-durable core NATS subscription
 * (ADR-0042).
 */
export function SubscriptionModal({ spaceId, opened, onClose }: { spaceId: string; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const templates = useTemplates()
  const conns = useConnections()
  const [error, setError] = useState('')
  const published = (templates.data ?? []).flatMap((t) => t.versions.filter((v) => v.status === 'published').map((v) => ({ t, v })))

  const form = useForm({
    defaultValues: { name: '', connection: '', source: 'template' as 'template' | 'custom', version: '', custom: definitionText(starterDefinition), follow: false, vars: {} as Record<string, string>, policy: 'new' as Cursor['policy'], time: '', seq: '',
      delivery: 'durable' as 'durable' | 'core', whilePaused: 'defer' as Subscription['while_paused'] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const core = value.delivery === 'core'
        const start: Cursor = { policy: core ? 'new' : value.policy, replay: false, generation: 0 }
        if (!core && value.policy === 'time') start.time = new Date(value.time).toISOString()
        if (!core && value.policy === 'seq') start.seq = Number(value.seq)
        const source = value.source === 'custom'
          ? { custom: parseDefinition(value.custom).def as Definition }
          : { template_version_id: value.version, follow: value.follow ? ('latest' as const) : ('pinned' as const) }
        const sub = unwrap(await api.POST('/spaces/{space_id}/subscriptions', {
          params: { path: { space_id: spaceId } },
          body: { name: value.name, connection_id: value.connection, ...source, vars: value.vars, start, delivery: value.delivery, while_paused: value.whilePaused },
        }))
        void qc.invalidateQueries({ queryKey: keys.subscriptions(spaceId) })
        form.reset()
        onClose()
        void navigate({ to: '/spaces/$spaceId/events/$subscriptionId', params: { spaceId, subscriptionId: sub.id } })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  // Variables belong to one definition: when it changes, drop their values and validation state.
  // With keep, only the variables no longer defined are dropped (the custom definition is being edited).
  const clearVars = (keep?: string[]) => {
    for (const k of Object.keys(form.state.fieldMeta)) if (k.startsWith('vars.') && !keep?.includes(k.slice(5))) form.deleteField(k as `vars.${string}`)
    if (!keep) form.setFieldValue('vars', {})
  }

  return (
    <Modal opened={opened} onClose={onClose} title="Subscribe to events" size="lg">
      <form id={FORM_ID} onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          {conns.isSuccess && conns.data.length === 0 && <Alert color="owlYellow">No Pulse connection exists yet. An administrator adds one under Events → Connections.</Alert>}
          <SimpleGrid cols={2}>
            <form.Field name="name" validators={{ onBlur: required('Name') }}>
              {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="connection" validators={{ onSubmit: required('Connection') }}>
              {(f) => <Select label="Connection" value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')} error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
                data={(conns.data ?? []).map((c) => ({ value: c.id, label: c.name, disabled: !c.enabled }))} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="source">
            {(f) => (
              <Stack gap={4}>
                <Text size="sm" fw={500} id="subscription-source-label">Definition</Text>
                <SegmentedControl value={f.state.value} aria-labelledby="subscription-source-label"
                  onChange={(v) => { f.handleChange(v as 'template' | 'custom'); clearVars(); void form.validateField('custom', 'change') }}
                  data={[{ value: 'template', label: 'Published template' }, { value: 'custom', label: 'Custom definition' }]} />
              </Stack>
            )}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.source}>
            {(source) => source === 'custom' ? (
              <Stack gap="xs">
                <Text size="xs" c="dimmed">A custom definition belongs to this subscription only: it is not in the template catalog and has no versions. It is validated by the server like a template version before the subscription starts.</Text>
                <Select label="Copy from a template" description="Optional: start from a published template's definition (replaces the editor's content)" searchable clearable
                  value={null} onChange={(vid) => {
                    const sel = published.find((p) => p.v.id === vid)
                    if (!sel) return
                    form.setFieldValue('custom', definitionText(sel.v.definition))
                    clearVars()
                  }}
                  data={published.map(({ t, v }) => ({ value: v.id, label: `${t.name} · v${v.version}` }))} />
                <form.Field name="custom" validators={{
                  onChange: ({ value, fieldApi }) => (fieldApi.form.getFieldValue('source') === 'custom' ? definitionError({ value }) : undefined),
                  onSubmit: ({ value, fieldApi }) => (fieldApi.form.getFieldValue('source') === 'custom' ? definitionError({ value }) : undefined),
                }}>
                  {(f) => <JsonInput label="Custom definition" description={placeholderHelp} required autosize minRows={10} maxRows={22} classNames={{ input: 'mono' }}
                    value={f.state.value} onBlur={f.handleBlur} error={fieldError(f.state.meta)}
                    onChange={(v) => {
                      f.handleChange(v)
                      const names = (parseDefinition(v).def?.variables ?? []).map((x) => x?.name).filter((n): n is string => typeof n === 'string')
                      clearVars(names)
                    }} />}
                </form.Field>
              </Stack>
            ) : (
              <form.Field name="version" validators={{ onSubmit: ({ value, fieldApi }) => (fieldApi.form.getFieldValue('source') === 'template' ? required('Template')({ value }) : undefined) }}>
                {(f) => (
                  <Select label="Template" value={f.state.value || null} searchable error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
                    onChange={(v) => { f.handleChange(v ?? ''); clearVars() }}
                    data={published.map(({ t, v }) => ({ value: v.id, label: `${t.name} · v${v.version}` }))} />
                )}
              </form.Field>
            )}
          </form.Subscribe>
          <form.Subscribe selector={(s) => ({ source: s.values.source, vid: s.values.version, custom: s.values.custom })}>
            {({ source, vid, custom }) => {
              const sel = published.find((p) => p.v.id === vid)
              const def = source === 'custom' ? parseDefinition(custom).def : sel?.v.definition
              if (!def) return null
              return (
                <Stack gap="xs">
                  <Text size="xs" c="dimmed">Subject <span className="mono">{def.subject}</span></Text>
                  <SimpleGrid cols={2}>
                    {(def.variables ?? []).filter((v) => typeof v?.name === 'string').map((v) => (
                      <form.Field key={`${source}-${v.name}`} name={`vars.${v.name}` as `vars.${string}`} validators={{ onBlur: varError(v), onSubmit: varError(v) }}>
                        {(f) => <TextInput label={v.name} description={v.description} placeholder={v.example} classNames={{ input: 'mono' }} value={f.state.value ?? ''}
                          onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
                      </form.Field>
                    ))}
                  </SimpleGrid>
                  {source === 'template' && (
                    <form.Field name="follow">
                      {(f) => <Switch label="Follow new published versions of this template (when they keep the same subject)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
                    </form.Field>
                  )}
                </Stack>
              )
            }}
          </form.Subscribe>
          <form.Field name="delivery">
            {(f) => (
              <Stack gap={4}>
                <Text size="sm" fw={500}>Delivery</Text>
                <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as 'durable' | 'core')} aria-label="Delivery"
                  data={[{ value: 'durable', label: 'Durable (JetStream)' }, { value: 'core', label: 'Core NATS (non-durable)' }]} />
                {f.state.value === 'core'
                  ? <Alert color="owlYellow" title="Non-durable subscription">{coreExplanation} Use it only for sources that publish without JetStream. It needs the Pulse grant subscribe:&lt;subject&gt; and receives new messages only.</Alert>
                  : <Text size="xs" c="dimmed">Its own JetStream durable consumer: an event is acknowledged only after its item is stored, redelivered after failures, and can be replayed.</Text>}
              </Stack>
            )}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.delivery}>
            {(delivery) => delivery === 'core' ? null : (
              <form.Field name="policy">
                {(f) => (
                  <Stack gap={4}>
                    <Text size="sm" fw={500}>Start with</Text>
                    <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as Cursor['policy'])} aria-label="Start with"
                      data={[{ value: 'new', label: 'New events only' }, { value: 'all', label: 'Everything retained' }, { value: 'time', label: 'From a time' }, { value: 'seq', label: 'From a sequence' }]} />
                    {f.state.value !== 'new' && <Text size="xs" c="owlYellow">Delivering history is a replay: it is audited and needs the replay permission.</Text>}
                  </Stack>
                )}
              </form.Field>
            )}
          </form.Subscribe>
          <form.Subscribe selector={(s) => (s.values.delivery === 'core' ? 'new' : s.values.policy)}>
            {(p) => p === 'time' ? (
              <form.Field name="time">{(f) => <TextInput type="datetime-local" label="From" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            ) : p === 'seq' ? (
              <form.Field name="seq">{(f) => <TextInput label="Stream sequence" value={f.state.value} onChange={(e) => f.handleChange(e.target.value.replace(/\D/g, ''))} />}</form.Field>
            ) : null}
          </form.Subscribe>
          <form.Field name="whilePaused">
            {(f) => (
              <Select label="While the space is paused" allowDeselect={false} value={f.state.value} onChange={(v) => f.handleChange((v ?? 'defer') as Subscription['while_paused'])}
                description={whilePausedOptions.find((o) => o.value === f.state.value)?.description}
                data={whilePausedOptions.map((o) => ({ value: o.value, label: o.label }))} />
            )}
          </form.Field>
        </Stack>
      </form>
      <Stack gap="sm" mt="sm">
        <form.Subscribe selector={(s) => ({ source: s.values.source, custom: s.values.custom, vars: s.values.vars })}>
          {({ source, custom, vars }) => {
            if (source !== 'custom') return null
            const parsed = parseDefinition(custom)
            return (
              <Accordion variant="contained">
                <Accordion.Item value="preview">
                  <Accordion.Control>Test against a sample event</Accordion.Control>
                  <Accordion.Panel>
                    {parsed.def && parsed.errors.length === 0
                      ? <PreviewPanel key={custom} def={parsed.def} vars={vars} />
                      : <Text size="sm" c="dimmed">Fix the definition first: {parsed.errors[0]}</Text>}
                  </Accordion.Panel>
                </Accordion.Item>
              </Accordion>
            )
          }}
        </form.Subscribe>
        {error && <Text c="owlRed" size="sm" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" form={FORM_ID} loading={busy}>Subscribe</Button>}</form.Subscribe>
        </Group>
      </Stack>
    </Modal>
  )
}
