import { Button, Group, Modal, Select, SimpleGrid, Stack, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, ifMatch, unwrap, type Space } from '@/api/client'
import { keys, useNodes } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required, SLUG_RE } from '@/lib/form'

const POLICIES = [
  { value: 'interrupt', label: 'Interrupt runs (keep them resumable)' },
  { value: 'kill', label: 'Stop runs' },
]

/** Creates a space pinned to a node, or edits one (If-Match on the version: a concurrent edit is a conflict, not lost). */
export function SpaceFormModal({ space, opened, onClose, onSaved }: { space?: Space; opened: boolean; onClose: () => void; onSaved?: (s: Space) => void }) {
  const qc = useQueryClient()
  const nodes = useNodes()
  const [error, setError] = useState('')
  const [key] = useState(idempotencyKey)
  const editing = !!space

  const form = useForm({
    defaultValues: {
      slug: space?.slug ?? '',
      name: space?.name ?? '',
      node_id: space?.node_id ?? '',
      description: space?.description ?? '',
      goals: space?.goals ?? '',
      standing_instructions: space?.standing_instructions ?? '',
      disconnect_policy: (space?.disconnect_policy ?? 'interrupt') as Space['disconnect_policy'],
    },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const saved = editing
          ? unwrap(
              await api.PUT('/spaces/{space_id}', {
                params: { path: { space_id: space.id }, header: { 'If-Match': ifMatch(space.version) } },
                body: { name: value.name, description: value.description, goals: value.goals, standing_instructions: value.standing_instructions, disconnect_policy: value.disconnect_policy },
              }),
            )
          : unwrap(await api.POST('/spaces', { params: { header: { 'Idempotency-Key': key } }, body: value }))
        qc.setQueryData(keys.space(saved.id), saved)
        void qc.invalidateQueries({ queryKey: keys.spaces })
        onSaved?.(saved)
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Modal opened={opened} onClose={onClose} title={editing ? `Edit ${space.name}` : 'New space'} size="lg">
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="sm">
          <SimpleGrid cols={2}>
            <form.Field name="name" validators={{ onBlur: required('Name') }}>
              {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="slug" validators={{ onBlur: ({ value }) => (!editing && !SLUG_RE.test(value) ? '3-48 lowercase letters, digits or dashes' : undefined) }}>
              {(f) => <TextInput label="Slug" disabled={editing} classNames={{ input: "mono" }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="node_id" validators={{ onSubmit: required('Node') }}>
              {(f) => (
                <Select label="Pinned node" disabled={editing} description={editing ? 'Relocation is a separate operation' : undefined} value={f.state.value || null}
                  onChange={(v) => f.handleChange(v ?? '')} error={f.state.meta.errors[0] ? String(f.state.meta.errors[0]) : undefined}
                  data={(nodes.data ?? []).filter((n) => n.state === 'active' || n.state === 'pending').map((n) => ({ value: n.id, label: `${n.name}${n.connected ? '' : ' (offline)'}` }))} />
              )}
            </form.Field>
            <form.Field name="disconnect_policy">
              {(f) => <Select label="When the node loses central" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Space['disconnect_policy'])} data={POLICIES} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="description">
            {(f) => <TextInput label="Description" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="goals">
            {(f) => <Textarea label="Goals" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="standing_instructions">
            {(f) => <Textarea label="Standing instructions" description="Given to every agent in this space" autosize minRows={3} maxRows={12} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="red" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>{editing ? 'Save' : 'Create space'}</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
