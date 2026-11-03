import { Button, Group, Select, SimpleGrid, Stack, Text } from '@mantine/core'
import { IconUserShare } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Task } from '@/api/client'
import { keys, useRuns, useSpaceRoles } from '@/api/queries'
import { errorMessage } from '@/components/ErrorAlert'
import { notifyOk } from '@/lib/notify'

const ENDED = ['exited', 'failed', 'lost']

/**
 * Reassigns a task (SM-CTRL-004): the role that should work on it and, optionally, the live run that holds it. The task
 * is saved whole at the version shown (If-Match), so a concurrent change is refused rather than overwritten. Reassigning
 * never stops or starts a run; control reads the new assignee the next time it looks at the queue.
 */
export function ReassignForm({ spaceId, task }: { spaceId: string; task: Task }) {
  const qc = useQueryClient()
  const roles = useSpaceRoles(spaceId).data ?? []
  const runs = (useRuns(spaceId).data ?? []).filter((r) => !ENDED.includes(r.observed) || r.id === task.assignee_run)
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { role: task.assignee_role ?? '', run: task.assignee_run ?? '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        unwrap(await api.PUT('/spaces/{space_id}/tasks/{task_id}', {
          params: { path: { space_id: spaceId, task_id: task.id }, header: { 'If-Match': ifMatch(task.version) } },
          body: { title: task.title, objective: task.objective, acceptance: task.acceptance, priority: task.priority, exclusions: task.exclusions,
            workspace: task.workspace, assignee_role: value.role, assignee_run: value.run },
        }))
        void qc.invalidateQueries({ queryKey: keys.queue(spaceId) })
        notifyOk('Task reassigned')
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  const roleNames = [...new Set([...roles.map((r) => r.name), ...(task.assignee_role ? [task.assignee_role] : [])])]
  return (
    <form aria-label="Reassign the task" onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
      <Stack gap={6}>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Assignee</Text>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          <form.Field name="role">
            {(f) => <Select label="Role" placeholder="Any role" clearable searchable value={f.state.value || null} onChange={(v) => f.handleChange(v ?? '')}
              data={roleNames.map((n) => ({ value: n, label: n }))} />}
          </form.Field>
          <form.Field name="run">
            {(f) => <Select label="Run" placeholder="No run" clearable searchable value={f.state.value || null}
              onChange={(v) => {
                f.handleChange(v ?? '')
                // A chosen run brings its role along, so the two never disagree.
                const r = runs.find((x) => x.id === v)
                if (r) form.setFieldValue('role', r.role)
              }}
              data={runs.map((r) => ({ value: r.id, label: `${r.role} · ${r.kind} · ${r.observed} · ${r.id.slice(-8)}` }))} />}
          </form.Field>
        </SimpleGrid>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <form.Subscribe selector={(s) => [s.isSubmitting, s.values.role === (task.assignee_role ?? '') && s.values.run === (task.assignee_run ?? '')] as const}>
          {([busy, same]) => (
            <Group justify="flex-end">
              <Button type="submit" variant="default" leftSection={<IconUserShare size={14} />} loading={busy} disabled={same}>Reassign</Button>
            </Group>
          )}
        </form.Subscribe>
      </Stack>
    </form>
  )
}
