import { Button, Group, JsonInput, Modal, Stack, Text, TextInput } from '@mantine/core'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Definition } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'

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

function parse(text: string): Definition {
  return JSON.parse(text) as Definition
}

/** Creates a template with a first draft (definitions are edited as JSON; the server validates them). */
export function TemplateCreateModal({ opened, onClose }: { opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [slug, setSlug] = useState('')
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [def, setDef] = useState(JSON.stringify(starterDefinition, null, 2))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setError('')
    setBusy(true)
    try {
      const t = unwrap(await api.POST('/event-templates', { body: { slug, name, description, definition: parse(def) } }))
      void qc.invalidateQueries({ queryKey: keys.templates })
      onClose()
      void navigate({ to: '/events/templates/$templateId', params: { templateId: t.id } })
    } catch (e) {
      setError(e instanceof SyntaxError ? 'The definition is not valid JSON' : errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="New event template" size="xl">
      <Stack gap="sm">
        <Group grow>
          <TextInput label="Slug" classNames={{ input: 'mono' }} value={slug} onChange={(e) => setSlug(e.target.value)} />
          <TextInput label="Name" value={name} onChange={(e) => setName(e.target.value)} />
        </Group>
        <TextInput label="Description" value={description} onChange={(e) => setDescription(e.target.value)} />
        <JsonInput label="Definition" description="Placeholders are {{event.…}}, {{data.…}} and {{vars.…}} with filters default, truncate, json, upper, lower, oneline and code."
          autosize minRows={12} maxRows={24} classNames={{ input: 'mono' }} value={def} onChange={setDef} />
        {error && <Text c="owlRed" size="sm" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>Create draft</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** Edits a template's draft (or starts one from a published version). */
export function DraftModal({ templateId, initial, opened, onClose }: { templateId: string; initial: Definition; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const [def, setDef] = useState(JSON.stringify(initial, null, 2))
  const [notes, setNotes] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setError('')
    setBusy(true)
    try {
      unwrap(await api.PUT('/event-templates/{template_id}/draft', { params: { path: { template_id: templateId } }, body: { definition: parse(def), notes } }))
      void qc.invalidateQueries({ queryKey: keys.template(templateId) })
      void qc.invalidateQueries({ queryKey: keys.templates })
      onClose()
    } catch (e) {
      setError(e instanceof SyntaxError ? 'The definition is not valid JSON' : errorMessage(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal opened={opened} onClose={onClose} title="Edit draft" size="xl">
      <Stack gap="sm">
        <JsonInput label="Definition" autosize minRows={14} maxRows={28} classNames={{ input: 'mono' }} value={def} onChange={setDef} />
        <TextInput label="Notes" description="What changed" value={notes} onChange={(e) => setNotes(e.target.value)} />
        {error && <Text c="owlRed" size="sm" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} onClick={() => void save()}>Save draft</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
