import { Button, Group, JsonInput, Modal, Stack, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Definition } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { definitionError, definitionText, parseDefinition, starterDefinition } from './definition'

export { starterDefinition }

/** The server's template slug rule (internal/events/templates.go reSlug). */
const TEMPLATE_SLUG = /^[a-z0-9][a-z0-9-]{1,62}$/

export const placeholderHelp = 'Placeholders are {{event.…}}, {{data.…}} and {{vars.…}} with filters default, truncate, json, upper, lower, oneline and code.'

/** Creates a template with a first draft (definitions are edited as JSON; the server validates them). */
export function TemplateCreateModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { slug: '', name: '', description: '', definition: definitionText(starterDefinition) },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const t = unwrap(await api.POST('/event-templates', {
          body: { slug: value.slug, name: value.name, description: value.description, definition: parseDefinition(value.definition).def as Definition },
        }))
        void qc.invalidateQueries({ queryKey: keys.templates })
        form.reset()
        onClose()
        void navigate({ to: '/events/templates/$templateId', params: { templateId: t.id } })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title="New event template" size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Group grow align="flex-start">
            <form.Field name="slug" validators={{ onBlur: ({ value }) => (!value ? 'Slug is required' : TEMPLATE_SLUG.test(value) ? undefined : 'Lowercase letters, digits and dashes (2–63)') }}>
              {(f) => <TextInput label="Slug" required classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="name" validators={{ onBlur: required('Name') }}>
              {(f) => <TextInput label="Name" required value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
          </Group>
          <form.Field name="description">
            {(f) => <TextInput label="Description" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="definition" validators={{ onChange: definitionError, onSubmit: definitionError }}>
            {(f) => <JsonInput label="Definition" description={placeholderHelp} autosize minRows={12} maxRows={24} classNames={{ input: 'mono' }}
              value={f.state.value} onBlur={f.handleBlur} onChange={f.handleChange} error={fieldError(f.state.meta)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Create draft</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}

/** Edits a template's draft (or starts one from a published version). */
export function DraftModal({ templateId, initial, opened, onClose }: { templateId: string; initial: Definition; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { definition: definitionText(initial), notes: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.PUT('/event-templates/{template_id}/draft', {
          params: { path: { template_id: templateId } }, body: { definition: parseDefinition(value.definition).def as Definition, notes: value.notes },
        }))
        void qc.invalidateQueries({ queryKey: keys.template(templateId) })
        void qc.invalidateQueries({ queryKey: keys.templates })
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened={opened} onClose={onClose} title="Edit draft" size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="definition" validators={{ onChange: definitionError, onSubmit: definitionError }}>
            {(f) => <JsonInput label="Definition" description={placeholderHelp} autosize minRows={14} maxRows={28} classNames={{ input: 'mono' }}
              value={f.state.value} onBlur={f.handleBlur} onChange={f.handleChange} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="notes">
            {(f) => <TextInput label="Notes" description="What changed" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Save draft</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
