import { Badge, Button, Card, Group, Modal, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type Prompt } from '@/api/client'
import { keys, useMe, usePrompts } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { hasGlobal } from '@/lib/access'
import { fieldError, required } from '@/lib/form'

export const Route = createFileRoute('/prompts/')({ component: PromptsPage })

export const kindLabel: Record<Prompt['kind'], string> = { role: 'Role', dream: 'Dream', kb: 'Knowledge base', event: 'Event', general: 'General' }

/** The central prompt catalog (SM-PROMPT-001): one versioning system for roles, dreams, KB and event instructions. */
function PromptsPage() {
  const prompts = usePrompts()
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [creating, setCreating] = useState(false)
  return (
    <>
      <PageHeader title="Prompts" description="Versioned prompts for roles, dreams, knowledge bases and events. Published versions never change; roles follow the latest or pin one."
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New prompt</Button>} />
      <ErrorAlert error={prompts.error} />
      <Card p={0}>
        <Table>
          <Table.Thead><Table.Tr><Table.Th>Prompt</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Latest</Table.Th><Table.Th>Provenance</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(prompts.data ?? []).map((p) => (
              <Table.Tr key={p.id}>
                <Table.Td>
                  <AnchorLink to="/prompts/$promptId" params={{ promptId: p.id }} size="sm" fw={500}>{p.name}</AnchorLink>
                  <Text size="xs" c="dimmed" className="mono">{p.slug}</Text>
                </Table.Td>
                <Table.Td><Badge variant="light">{kindLabel[p.kind]}</Badge></Table.Td>
                <Table.Td>{p.latest > 0 ? <Text size="sm">v{p.latest}</Text> : <Text size="sm" c="dimmed">draft only</Text>}</Table.Td>
                <Table.Td><Text size="xs" c="dimmed">{p.forked_from ? `forked from ${p.forked_from}` : p.description}</Text></Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {prompts.isSuccess && prompts.data.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No prompts yet.</Text>}
      </Card>
      {creating && <NewPromptModal onClose={() => setCreating(false)} />}
    </>
  )
}

function NewPromptModal({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { slug: '', name: '', kind: 'role' as Prompt['kind'], body: '' },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        const p = unwrap(await api.POST('/prompts', { body: value }))
        void qc.invalidateQueries({ queryKey: keys.prompts })
        onClose()
        void navigate({ to: '/prompts/$promptId', params: { promptId: p.id } })
      } catch (e) {
        setError(errorMessage(e))
      }
    },
  })
  return (
    <Modal opened onClose={onClose} title="New prompt" size="lg">
      <form onSubmit={(e) => { e.preventDefault(); void form.handleSubmit() }}>
        <Stack gap="sm">
          <Group grow>
            <form.Field name="name" validators={{ onBlur: required('Name') }}>
              {(f) => <TextInput label="Name" value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
            <form.Field name="slug" validators={{ onChange: ({ value }) => (!/^[a-z0-9][a-z0-9-]{0,62}$/.test(value) ? 'Lowercase letters, digits and dashes' : undefined) }}>
              {(f) => <TextInput label="Slug" classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={(e) => f.handleChange(e.target.value)} error={fieldError(f.state.meta)} />}
            </form.Field>
          </Group>
          <form.Field name="kind">
            {(f) => <Select label="Kind" allowDeselect={false} value={f.state.value} onChange={(v) => v && f.handleChange(v as Prompt['kind'])}
              data={Object.entries(kindLabel).map(([value, label]) => ({ value, label }))} />}
          </form.Field>
          <form.Field name="body">
            {(f) => <Textarea label="First draft" description="Markdown; publish it to use it" autosize minRows={6} maxRows={16} classNames={{ input: 'mono' }}
              value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
          </form.Field>
          {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
          <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button type="submit">Create</Button></Group>
        </Stack>
      </form>
    </Modal>
  )
}
