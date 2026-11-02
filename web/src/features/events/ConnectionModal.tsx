import { Button, Group, Modal, Stack, Switch, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type EventConnection } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError } from '@/lib/form'

/** Adds or edits a Pulse connection. Credentials are write-only: editing without new ones keeps the stored ones. */
export function ConnectionModal({ conn, opened, onClose }: { conn?: EventConnection; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { name: conn?.name ?? '', url: conn?.url ?? 'tls://', creds: '', ca_pem: '', stream: conn?.stream ?? 'EVENTS', enabled: conn?.enabled ?? true },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const body = { ...value, creds: value.creds || undefined, ca_pem: value.ca_pem || undefined }
        if (conn) unwrap(await api.PUT('/event-connections/{connection_id}', { params: { path: { connection_id: conn.id }, header: { 'If-Match': ifMatch(conn.version) } }, body }))
        else unwrap(await api.POST('/event-connections', { body }))
        void qc.invalidateQueries({ queryKey: keys.connections })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title={conn ? `Edit ${conn.name}` : 'Add a Pulse connection'} size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="name" validators={{ onBlur: ({ value }) => (!/^[a-z0-9][a-z0-9-]{1,62}$/.test(value) ? 'Lowercase letters, digits and dashes' : undefined) }}>
            {(f) => <TextInput label="Name" classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="url">
            {(f) => <TextInput label="URL" description="nats://host:port or tls://host:port" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="creds">
            {(f) => (
              <Textarea label="Credentials (.creds)" description={conn ? 'Leave empty to keep the stored credentials' : 'The .creds file Pulse issued for SpaceMan; stored encrypted, never shown again'}
                autosize minRows={4} maxRows={8} classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />
            )}
          </form.Field>
          <form.Field name="ca_pem">
            {(f) => <Textarea label="CA certificate (PEM)" description="Only for a private CA" autosize minRows={2} maxRows={6} classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <Group grow>
            <form.Field name="stream">
              {(f) => <TextInput label="Stream" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <form.Field name="enabled">
              {(f) => <Switch mt="lg" label="Enabled" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
            </form.Field>
          </Group>
          {error && <Text c="red" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>{conn ? 'Save' : 'Add connection'}</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

