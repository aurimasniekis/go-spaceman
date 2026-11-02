import { Alert, Button, Group, Modal, SegmentedControl, Select, SimpleGrid, Stack, Switch, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Cursor } from '@/api/client'
import { keys, useConnections, useTemplates } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'

/**
 * Subscribes a space to a published template version (SM-EVT-005). Variable fields come from the template; the server
 * binds and validates everything before the subscription starts. By default only new events are delivered.
 */
export function SubscriptionModal({ spaceId, opened, onClose }: { spaceId: string; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const templates = useTemplates()
  const conns = useConnections()
  const [error, setError] = useState('')
  const published = (templates.data ?? []).flatMap((t) => t.versions.filter((v) => v.status === 'published').map((v) => ({ t, v })))

  const form = useForm({
    defaultValues: { name: '', connection: '', version: '', follow: false, vars: {} as Record<string, string>, policy: 'new' as Cursor['policy'], time: '', seq: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const start: Cursor = { policy: value.policy, replay: false, generation: 0 }
        if (value.policy === 'time') start.time = new Date(value.time).toISOString()
        if (value.policy === 'seq') start.seq = Number(value.seq)
        const sub = unwrap(await api.POST('/spaces/{space_id}/subscriptions', {
          params: { path: { space_id: spaceId } },
          body: { name: value.name, connection_id: value.connection, template_version_id: value.version, follow: value.follow ? 'latest' : 'pinned', vars: value.vars, start },
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

  return (
    <Modal opened={opened} onClose={onClose} title="Subscribe to events" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
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
          <form.Field name="version" validators={{ onSubmit: required('Template') }}>
            {(f) => (
              <Select label="Template" value={f.state.value || null} searchable error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
                onChange={(v) => { f.handleChange(v ?? ''); form.setFieldValue('vars', {}) }}
                data={published.map(({ t, v }) => ({ value: v.id, label: `${t.name} · v${v.version}` }))} />
            )}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.version}>
            {(vid) => {
              const sel = published.find((p) => p.v.id === vid)
              if (!sel) return null
              return (
                <Stack gap="xs">
                  <Text size="xs" c="dimmed">Subject <span className="mono">{sel.v.definition.subject}</span></Text>
                  <SimpleGrid cols={2}>
                    {sel.v.definition.variables.map((v) => (
                      <form.Field key={v.name} name={`vars.${v.name}` as `vars.${string}`}
                        validators={{ onBlur: ({ value }) => (v.required && !value ? `${v.name} is required` : v.pattern && value && !new RegExp(`^(?:${v.pattern})$`).test(value) ? `Must match ${v.pattern}` : undefined) }}>
                        {(f) => <TextInput label={v.name} description={v.description} placeholder={v.example} classNames={{ input: 'mono' }} value={f.state.value ?? ''}
                          onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
                      </form.Field>
                    ))}
                  </SimpleGrid>
                  <form.Field name="follow">
                    {(f) => <Switch label="Follow new published versions of this template (when they keep the same subject)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
                  </form.Field>
                </Stack>
              )
            }}
          </form.Subscribe>
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
          <form.Subscribe selector={(s) => s.values.policy}>
            {(p) => p === 'time' ? (
              <form.Field name="time">{(f) => <TextInput type="datetime-local" label="From" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
            ) : p === 'seq' ? (
              <form.Field name="seq">{(f) => <TextInput label="Stream sequence" value={f.state.value} onChange={(e) => f.handleChange(e.target.value.replace(/\D/g, ''))} />}</form.Field>
            ) : null}
          </form.Subscribe>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Subscribe</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
