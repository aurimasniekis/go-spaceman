import { Button, Group, Modal, SegmentedControl, Select, SimpleGrid, Stack, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type Task } from '@/api/client'
import { keys, useQueue } from '@/api/queries'
import { Composer } from '@/components/composer/Composer'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'

export interface TaskSeed {
  title?: string
  objective?: string
  fromInboxItem?: string
  parentId?: string
}

/** Adds a task. Priority and the queue policy decide where it lands; operators move it afterwards. */
export function TaskModal({ spaceId, seed, opened, onClose, onCreated }: { spaceId: string; seed?: TaskSeed; opened: boolean; onClose: () => void; onCreated?: (t: Task) => void }) {
  const qc = useQueryClient()
  const queue = useQueue(spaceId)
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const [composerKey, setComposerKey] = useState(0)
  const form = useForm({
    defaultValues: {
      title: seed?.title ?? '', objective: seed?.objective ?? '', acceptance: '', priority: 'normal' as Task['priority'], kind: 'ordinary' as Task['kind'],
      status: 'ready' as 'ready' | 'backlog', exclusions: [] as string[], parent: seed?.parentId ?? '', dependsOn: [] as string[], attachmentIds: [] as string[],
    },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const t = unwrap(await api.POST('/spaces/{space_id}/tasks', {
          params: { path: { space_id: spaceId }, header: { 'Idempotency-Key': key } },
          body: { title: value.title, objective: value.objective, acceptance: value.acceptance, priority: value.priority, kind: value.kind, status: value.status,
            exclusions: value.exclusions, parent_id: value.parent || undefined, depends_on: value.dependsOn, from_inbox_item: seed?.fromInboxItem,
            attachment_ids: value.attachmentIds.length > 0 ? value.attachmentIds : undefined },
        }))
        void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
        void qc.invalidateQueries({ queryKey: keys.inbox(spaceId) })
        setKey(idempotencyKey())
        form.reset()
        setComposerKey((k) => k + 1)
        onClose()
        onCreated?.(t)
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const open = (queue.data?.tasks ?? []).filter((t) => !['done', 'cancelled', 'failed'].includes(t.status))
  return (
    <Modal opened={opened} onClose={onClose} title={seed?.fromInboxItem ? 'Create a task from this item' : 'New task'} size="xl">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="title" validators={{ onBlur: required('Title') }}>
            {(f) => <TextInput label="Title" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <Stack gap={4}>
            <Text size="sm" fw={500} id="task-objective-label">Objective</Text>
            <Text size="xs" c="dimmed">Markdown; paste, drop or attach images and files</Text>
            <Composer key={composerKey} spaceId={spaceId} ariaLabel="Objective" initialMarkdown={seed?.objective} minHeight={120}
              onSubmit={() => void form.handleSubmit()}
              onChange={(v) => { form.setFieldValue('objective', v.markdown); form.setFieldValue('attachmentIds', v.attachmentIds) }} />
          </Stack>
          <form.Field name="acceptance">
            {(f) => <Textarea label="Acceptance criteria" description="Markdown; control decides when they are met" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <SimpleGrid cols={{ base: 1, sm: 3 }}>
            <form.Field name="priority">
              {(f) => <Select label="Priority" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Task['priority'])}
                data={['low', 'normal', 'high', 'urgent'].map((p) => ({ value: p, label: p }))} />}
            </form.Field>
            <form.Field name="kind">
              {(f) => (
                <Stack gap={2}><Text size="sm" fw={500}>Kind</Text>
                  <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as Task['kind'])} data={[{ value: 'ordinary', label: 'Ordinary' }, { value: 'incident', label: 'Incident' }]} />
                </Stack>
              )}
            </form.Field>
            <form.Field name="status">
              {(f) => (
                <Stack gap={2}><Text size="sm" fw={500}>Start in</Text>
                  <SegmentedControl value={f.state.value} onChange={(v) => f.handleChange(v as 'ready' | 'backlog')} data={[{ value: 'ready', label: 'Ready' }, { value: 'backlog', label: 'Backlog' }]} />
                </Stack>
              )}
            </form.Field>
          </SimpleGrid>
          <SimpleGrid cols={{ base: 1, sm: 2 }}>
            <form.Field name="parent">
              {(f) => <Select label="Parent task" clearable searchable value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')}
                data={open.map((t) => ({ value: t.id, label: t.title }))} />}
            </form.Field>
            <form.Field name="dependsOn">
              {(f) => <TagsInput label="Depends on" description="Task titles" value={f.state.value.map((id) => open.find((t) => t.id === id)?.title ?? id)}
                data={open.map((t) => t.title)} onChange={(titles) => f.handleChange(titles.map((x) => open.find((t) => t.title === x)?.id).filter((x): x is string => !!x))} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="exclusions">
            {(f) => <TagsInput label="Exclusive resources" description="Keys like env:staging or repo:api; tasks sharing one never run at the same time" value={f.state.value} onChange={f.handleChange} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => s.isSubmitting}>{(busy) => <Button type="submit" loading={busy}>Add to queue</Button>}</form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
