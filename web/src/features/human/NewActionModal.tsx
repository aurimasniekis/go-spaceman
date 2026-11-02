import { Button, Checkbox, Group, Modal, SegmentedControl, Select, Stack, TagsInput, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type HumanAction, type HumanType } from '@/api/client'
import { keys, useQueue, useSpaceHumanTypes } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError, required } from '@/lib/form'
import { typeLabel } from './badges'
import { SchemaForm } from './SchemaForm'

/** Creates a human action as the signed-in operator (agents create theirs through their tools). */
export function NewActionModal({ spaceId, opened, onClose }: { spaceId: string; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const queue = useQueue(spaceId)
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const custom = useSpaceHumanTypes(spaceId).data ?? []
  const [data, setData] = useState<Record<string, unknown>>({})
  const form = useForm({
    defaultValues: {
      type: 'todo' as string, title: '', body: '', priority: 'normal' as HumanAction['priority'], options: [] as string[], allowText: true,
      approvalAction: '', steps: [] as string[], requiresAck: true, taskId: '', blocks: false,
    },
    onSubmit: async ({ value: v }) => {
      setError('')
      const slug = v.type.startsWith('custom:') ? v.type.slice(7) : ''
      const request = slug ? data : v.type === 'question' ? { options: v.options, allow_text: v.allowText }
        : v.type === 'approval' ? { action: v.approvalAction } : v.type === 'todo' ? { steps: v.steps } : { requires_ack: v.requiresAck }
      try {
        unwrap(await api.POST('/spaces/{space_id}/human-actions', {
          params: { path: { space_id: spaceId }, header: { 'Idempotency-Key': key } },
          body: { type: (slug ? 'custom' : v.type) as HumanType, type_slug: slug || undefined, title: v.title, body: v.body || undefined,
            priority: slug ? undefined : v.priority, request, task_id: v.taskId || undefined, blocks_task: v.taskId ? v.blocks : undefined },
        }))
        void qc.invalidateQueries({ queryKey: keys.human(spaceId) })
        void qc.invalidateQueries({ queryKey: keys.humanPending })
        setKey(idempotencyKey())
        form.reset()
        onClose()
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const tasks = (queue.data?.tasks ?? []).filter((t) => !['done', 'cancelled', 'failed'].includes(t.status))
  return (
    <Modal opened={opened} onClose={onClose} title="New human action" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <form.Field name="type">
            {(f) => custom.length === 0 ? (
              <SegmentedControl fullWidth value={f.state.value} onChange={(v) => f.handleChange(v)}
                data={(['question', 'approval', 'todo', 'message'] as const).map((t) => ({ value: t, label: typeLabel[t] }))} />
            ) : (
              <Select label="Type" allowDeselect={false} value={f.state.value} onChange={(v) => { if (v) { f.handleChange(v); setData({}) } }}
                data={[{ group: 'Built-in', items: (['question', 'approval', 'todo', 'message'] as const).map((t) => ({ value: t, label: typeLabel[t] })) },
                  { group: 'This space', items: custom.map((c) => ({ value: `custom:${c.slug}`, label: `${c.label} (v${c.number})` })) }]} />
            )}
          </form.Field>
          <form.Field name="title" validators={{ onBlur: required('Title') }}>
            {(f) => <TextInput label="Title" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
          </form.Field>
          <form.Field name="body">
            {(f) => <Textarea label="Details" description="Markdown" autosize minRows={3} maxRows={10} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Subscribe selector={(s) => s.values.type}>
            {(type) => (
              <>
                {type === 'question' && (
                  <>
                    <form.Field name="options">
                      {(f) => <TagsInput label="Offered answers" description="Optional; press Enter after each" value={f.state.value} onChange={f.handleChange} />}
                    </form.Field>
                    <form.Field name="allowText">
                      {(f) => <Checkbox label="Allow a written answer" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
                    </form.Field>
                  </>
                )}
                {type === 'approval' && (
                  <form.Field name="approvalAction" validators={{ onBlur: required('The requested action') }}>
                    {(f) => <TextInput label="What needs approval" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
                  </form.Field>
                )}
                {type === 'todo' && (
                  <form.Field name="steps">
                    {(f) => <TagsInput label="Steps" description="Optional checklist; press Enter after each" value={f.state.value} onChange={f.handleChange} />}
                  </form.Field>
                )}
                {type.startsWith('custom:') && (() => {
                  const c = custom.find((x) => `custom:${x.slug}` === type)
                  return c ? <SchemaForm label="Request" schema={c.request_schema} ui={c.ui} value={data} onChange={setData} /> : null
                })()}
                {type === 'message' && (
                  <form.Field name="requiresAck">
                    {(f) => <Checkbox label="Needs an acknowledgement (otherwise informational, never blocking)" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
                  </form.Field>
                )}
              </>
            )}
          </form.Subscribe>
          <Group grow align="flex-start">
            <form.Field name="priority">
              {(f) => <Select label="Priority" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as HumanAction['priority'])}
                data={['low', 'normal', 'high', 'urgent']} />}
            </form.Field>
            <form.Field name="taskId">
              {(f) => <Select label="Related task" clearable searchable value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')}
                data={tasks.map((t) => ({ value: t.id, label: t.title }))} />}
            </form.Field>
          </Group>
          <form.Subscribe selector={(s) => [s.values.taskId, s.values.type, s.values.requiresAck] as const}>
            {([taskId, type, ack]) => taskId && (type !== 'message' || ack) ? (
              <form.Field name="blocks">
                {(f) => <Checkbox label="Block that task until someone responds" checked={f.state.value} onChange={(e) => f.handleChange(e.currentTarget.checked)} />}
              </form.Field>
            ) : null}
          </form.Subscribe>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button type="submit">Create</Button></Group>
        </Stack>
      </form>
    </Modal>
  )
}
