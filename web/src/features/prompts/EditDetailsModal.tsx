import { Button, Group, Modal, Stack, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ApiError, ifMatch, unwrap, type Prompt } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { useNarrow } from '@/lib/media'
import { notifyOk } from '@/lib/notify'

/** The conflict statuses of a stale If-Match: 412 (precondition failed) and 409. */
export const isConflict = (e: unknown) => e instanceof ApiError && (e.status === 412 || e.status === 409)

/** Changes a prompt's name and description in place (no new version); a stale version is a conflict. */
export function EditDetailsModal({ prompt, onClose }: { prompt: Prompt; onClose: () => void }) {
  const qc = useQueryClient()
  const narrow = useNarrow()
  const [error, setError] = useState('')
  const [stale, setStale] = useState(false)
  // Pinned when the dialog opens: the watch stream refetches the prompt, and a live version with stale values would overwrite someone else's edit.
  const [loaded] = useState(() => ({ version: prompt.version, name: prompt.name, description: prompt.description ?? '' }))
  const form = useForm({
    defaultValues: { name: loaded.name, description: loaded.description },
    onSubmit: async ({ value }) => {
      setError('')
      const body: { name?: string; description?: string } = {}
      if (value.name !== loaded.name) body.name = value.name
      if (value.description !== loaded.description) body.description = value.description
      if (Object.keys(body).length === 0) { onClose(); return }
      try {
        unwrap(await api.PATCH('/prompts/{prompt_id}', { params: { path: { prompt_id: prompt.id }, header: { 'If-Match': ifMatch(loaded.version) } }, body }))
        void qc.invalidateQueries({ queryKey: keys.prompt(prompt.id) })
        void qc.invalidateQueries({ queryKey: keys.prompts })
        notifyOk('Prompt details saved')
        onClose()
      } catch (e) {
        if (isConflict(e)) setStale(true)
        setError(isConflict(e) ? 'Someone else changed this prompt since you opened it. Close this dialog to load the latest version, then try again.' : errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title="Edit details" fullScreen={narrow}>
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Text size="sm">Changes the name and description in place and creates no version. The slug and kind never change.</Text>
          <form.Field name="name">{(f) => <TextInput label="Name" required value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}</form.Field>
          <form.Field name="description">
            {(f) => <Textarea label="Description" description="What the prompt is for" autosize minRows={2} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(st) => st.isSubmitting}>{(busy) => <Button type="submit" loading={busy} disabled={stale}>Save</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
