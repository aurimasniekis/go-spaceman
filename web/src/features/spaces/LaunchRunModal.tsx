import { Button, Group, Modal, Select, SimpleGrid, Stack, Text, Textarea, TextInput } from '@mantine/core'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, idempotencyKey, unwrap, type LaunchInput, type Node, type Space } from '@/api/client'
import { keys } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { fieldError } from '@/lib/form'

const ROLE_RE = /^[a-z][a-z0-9-]{0,47}$/

const KINDS: { value: LaunchInput['kind']; label: string }[] = [
  { value: 'control', label: 'Control — coordinates the space' },
  { value: 'worker', label: 'Worker — implements one task' },
  { value: 'diagnostic', label: 'Diagnostic — one-off investigation' },
]

/** Launches one run on the space's pinned node (SM-RUN-001). The idempotency key makes a retried submit safe. */
export function LaunchRunModal({ space, node, opened, onClose }: { space: Space; node?: Node; opened: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState('')
  const [key, setKey] = useState(idempotencyKey)
  const available = (node?.capabilities.harnesses ?? []).filter((h) => h.available).map((h) => h.kind)

  const form = useForm({
    defaultValues: { harness: (available[0] ?? 'claude-code') as LaunchInput['harness'], kind: 'control' as LaunchInput['kind'], role: 'control', model: '', prompt: '', system: '', workspace: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const run = unwrap(
          await api.POST('/spaces/{space_id}/runs', {
            params: { path: { space_id: space.id }, header: { 'Idempotency-Key': key } },
            body: { ...value, model: value.model || undefined, prompt: value.prompt || undefined, system: value.system || undefined, workspace: value.workspace || undefined },
          }),
        )
        void qc.invalidateQueries({ queryKey: keys.runs(space.id) })
        setKey(idempotencyKey())
        form.reset()
        onClose()
        void navigate({ to: '/spaces/$spaceId/runs/$runId', params: { spaceId: space.id, runId: run.id } })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Modal opened={opened} onClose={onClose} title={`Launch a run in ${space.name}`} size="lg">
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void form.handleSubmit()
        }}
      >
        <Stack gap="sm">
          {node && !node.connected && <Text c="owlYellow" size="sm">The pinned node {node.name} is offline; the launch waits until it reconnects.</Text>}
          <SimpleGrid cols={2}>
            <form.Field name="harness">
              {(f) => (
                <Select label="Harness" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as LaunchInput['harness'])}
                  data={[
                    { value: 'claude-code', label: `Claude Code${available.includes('claude-code') ? '' : ' (not detected)'}` },
                    { value: 'codex', label: `Codex${available.includes('codex') ? '' : ' (not detected)'}` },
                  ]} />
              )}
            </form.Field>
            <form.Field name="kind">
              {(f) => <Select label="Run kind" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as LaunchInput['kind'])} data={KINDS} />}
            </form.Field>
            <form.Field name="role" validators={{ onChange: ({ value }) => (!ROLE_RE.test(value) ? 'Lowercase letters, digits and dashes' : undefined) }}>
              {(f) => <TextInput label="Role" description="e.g. control, implementor-backend" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="model">
              {(f) => <TextInput label="Model" description="Harness default when empty" value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          </SimpleGrid>
          <form.Field name="prompt">
            {(f) => <Textarea label="Initial prompt" autosize minRows={3} maxRows={10} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="system">
            {(f) => <Textarea label="Additional system prompt" description="Appended after the space's standing instructions" autosize minRows={2} maxRows={8} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          <form.Field name="workspace">
            {(f) => <TextInput label="Workspace" description="Directory inside the space's workdir (empty: the workdir itself)" classNames={{ input: "mono" }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="red" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <form.Subscribe selector={(s) => [s.canSubmit, s.isSubmitting] as const}>
              {([can, busy]) => <Button type="submit" disabled={!can} loading={busy}>Launch</Button>}
            </form.Subscribe>
          </Group>
        </Stack>
      </form>
    </Modal>
  )
}
