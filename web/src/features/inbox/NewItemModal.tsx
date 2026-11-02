import { Button, Group, JsonInput, Modal, SegmentedControl, Select, SimpleGrid, Stack, Text, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type InboxItem } from '@/api/client'
import { keys } from '@/api/queries'
import { Composer } from '@/components/composer/Composer'
import { errorMessage } from '@/components/ErrorAlert'

const URGENCIES = [
  { value: 'low', label: 'Low' },
  { value: 'normal', label: 'Normal' },
  { value: 'high', label: 'High' },
  { value: 'urgent', label: 'Urgent' },
]

/**
 * Adds an operator item to a space's inbox (SM-UI-005: task entry belongs in the inbox). Markdown bodies use the
 * composer, with pasted, dropped or attached files; JSON bodies are validated before sending.
 */
export function NewItemModal({ spaceId, opened, onClose, onCreated }: { spaceId: string; opened: boolean; onClose: () => void; onCreated: (it: InboxItem) => void }) {
  const qc = useQueryClient()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const [composerKey, setComposerKey] = useState(0)
  const form = useForm({
    defaultValues: { title: '', urgency: 'normal' as InboxItem['urgency'], contentType: 'text/markdown' as InboxItem['content_type'], markdown: '', json: '', attachmentIds: [] as string[] },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const body = value.contentType === 'application/json' ? value.json : value.markdown
        const it = unwrap(
          await api.POST('/spaces/{space_id}/inbox', {
            params: { path: { space_id: spaceId }, header: { 'Idempotency-Key': key } },
            body: { title: value.title || undefined, urgency: value.urgency, content_type: value.contentType, body,
              attachment_ids: value.contentType === 'text/markdown' ? value.attachmentIds : undefined },
          }),
        )
        void qc.invalidateQueries({ queryKey: keys.inbox(spaceId) })
        setKey(idempotencyKey())
        form.reset()
        setComposerKey((k) => k + 1)
        onClose()
        onCreated(it)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Modal opened={opened} onClose={onClose} title="New inbox item" size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Text size="xs" c="dimmed">Control reads the inbox and decides what an item becomes: a task, a question for a person, a reply, or nothing.</Text>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="title">
              {(f) => <TextInput label="Title" description="Optional: defaults to the first line" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
            <form.Field name="urgency">
              {(f) => <Select label="Urgency" allowDeselect={false} data={URGENCIES} value={f.state.value} onChange={(v) => v && f.handleChange(v as InboxItem['urgency'])} />}
            </form.Field>
            <form.Field name="contentType">
              {(f) => (
                <Stack gap={2}>
                  <Text size="sm" fw={500}>Format</Text>
                  <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as InboxItem['content_type'])}
                    data={[{ value: 'text/markdown', label: 'Text' }, { value: 'application/json', label: 'JSON' }]} aria-label="Format" />
                </Stack>
              )}
            </form.Field>
          </SimpleGrid>
          <form.Subscribe selector={(s) => s.values.contentType}>
            {(ct) =>
              ct === 'application/json' ? (
                <form.Field name="json" validators={{ onBlur: ({ value }) => { try { JSON.parse(value); return undefined } catch { return 'Not valid JSON' } } }}>
                  {(f) => <JsonInput label="JSON" autosize minRows={8} formatOnBlur validationError="Not valid JSON" classNames={{ input: 'mono' }}
                    value={f.state.value} onChange={f.handleChange} onBlur={f.handleBlur} />}
                </form.Field>
              ) : (
                <Composer key={composerKey} spaceId={spaceId} ariaLabel="Item text" onSubmit={() => void form.handleSubmit()}
                  onChange={(v) => { form.setFieldValue('markdown', v.markdown); form.setFieldValue('attachmentIds', v.attachmentIds) }} />
              )
            }
          </form.Subscribe>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="space-between">
            <Text size="xs" c="dimmed">⌘/Ctrl + Enter to add</Text>
            <Group gap="xs">
              <Button variant="default" onClick={onClose}>Cancel</Button>
              <form.Subscribe selector={(s) => [s.isSubmitting, s.values.markdown, s.values.json, s.values.contentType] as const}>
                {([busy, md, json, ct]) => <Button type="submit" loading={busy} disabled={ct === 'text/markdown' ? !md.trim() : !json.trim()}>Add to inbox</Button>}
              </form.Subscribe>
            </Group>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
