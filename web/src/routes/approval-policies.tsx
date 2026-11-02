import { ActionIcon, Alert, Button, Card, Group, Modal, NumberInput, Select, Stack, Table, Text, Textarea, TextInput } from '@mantine/core'
import { IconInfoCircle, IconPlus, IconTrash } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, check, unwrap } from '@/api/client'
import { keys, useApprovalPolicies, useMe, useSpaces } from '@/api/queries'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { hasGlobal } from '@/lib/access'
import { notifyError } from '@/lib/notify'

export const Route = createFileRoute('/approval-policies')({ component: ApprovalPoliciesPage })

const dur = (s: number) => (s % 86400 === 0 ? `${s / 86400} d` : s % 3600 === 0 ? `${s / 3600} h` : `${Math.round(s / 60)} min`)

/** Which operations SpaceMan runs only after a person's approval (SM-APPR-001/002). */
function ApprovalPoliciesPage() {
  const qc = useQueryClient()
  const policies = useApprovalPolicies()
  const spaces = useSpaces()
  const canWrite = hasGlobal(useMe().data, 'admin')
  const [adding, setAdding] = useState(false)
  const spaceName = (id: string) => (id ? spaces.data?.find((s) => s.id === id)?.name ?? id : 'Every space')
  const remove = async (id: string) => {
    try {
      check(await api.DELETE('/approval-policies/{policy_id}', { params: { path: { policy_id: id } } }))
      void qc.invalidateQueries({ queryKey: keys.approvalPolicies })
    } catch (e) {
      notifyError(e, 'Could not remove the policy')
    }
  }
  return (
    <>
      <PageHeader title="Approval policies" description="Operations SpaceMan runs only after a person approves them, bound to the exact arguments, the requesting run, an expiry and a number of uses."
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setAdding(true)}>Add policy</Button>} />
      <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />} mb="md">
        Policies cover what SpaceMan itself controls: built-in agent tools (<code>spaceman.&lt;tool&gt;</code>) and tools of MCP servers behind the gateway
        (<code>mcp.&lt;server&gt;.&lt;tool&gt;</code>). They cannot intercept shell commands or a harness's own permission prompts.
      </Alert>
      <ErrorAlert error={policies.error} />
      <Card p={0}>
        <Table>
          <Table.Thead><Table.Tr><Table.Th>Operation</Table.Th><Table.Th>Space</Table.Th><Table.Th>Uses per approval</Table.Th><Table.Th>Usable for</Table.Th><Table.Th>Request waits</Table.Th><Table.Th /></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(policies.data ?? []).map((p) => (
              <Table.Tr key={p.id}>
                <Table.Td><Text size="sm" className="mono">{p.operation}</Text>{p.note && <Text size="xs" c="dimmed">{p.note}</Text>}</Table.Td>
                <Table.Td>{spaceName(p.space_id)}</Table.Td>
                <Table.Td>{p.uses}</Table.Td>
                <Table.Td>{dur(p.valid_seconds)}</Table.Td>
                <Table.Td>{dur(p.request_ttl_seconds)}</Table.Td>
                <Table.Td>{canWrite && <ActionIcon variant="subtle" aria-label={`Remove the policy for ${p.operation}`} onClick={() => void remove(p.id)}><IconTrash size={14} /></ActionIcon>}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
        {policies.isSuccess && policies.data.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No policies: every approval is advisory.</Text>}
      </Card>
      {adding && <PolicyModal onClose={() => setAdding(false)} spaces={(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))} />}
    </>
  )
}

function PolicyModal({ onClose, spaces }: { onClose: () => void; spaces: { value: string; label: string }[] }) {
  const qc = useQueryClient()
  const [v, setV] = useState({ space_id: '', operation: '', uses: 1, valid_minutes: 60, request_hours: 24, note: '' })
  const [error, setError] = useState('')
  const save = async () => {
    try {
      unwrap(await api.POST('/approval-policies', { body: { space_id: v.space_id || undefined, operation: v.operation, uses: v.uses, valid_seconds: v.valid_minutes * 60,
        request_ttl_seconds: v.request_hours * 3600, note: v.note || undefined } }))
      void qc.invalidateQueries({ queryKey: keys.approvalPolicies })
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    }
  }
  return (
    <Modal opened onClose={onClose} title="Add an approval policy">
      <Stack gap="sm">
        <TextInput label="Operation" description="spaceman.<tool>, mcp.<server>.<tool>, or a prefix such as mcp.deploys.*" placeholder="mcp.deploys.deploy" classNames={{ input: 'mono' }}
          value={v.operation} onChange={(e) => setV({ ...v, operation: e.target.value })} />
        <Select label="Space" data={[{ value: '', label: 'Every space' }, ...spaces]} value={v.space_id} onChange={(x) => setV({ ...v, space_id: x ?? '' })} allowDeselect={false} />
        <Group grow>
          <NumberInput label="Uses per approval" min={1} max={100} value={v.uses} onChange={(n) => setV({ ...v, uses: Number(n) || 1 })} />
          <NumberInput label="Usable for (minutes)" min={1} max={10080} value={v.valid_minutes} onChange={(n) => setV({ ...v, valid_minutes: Number(n) || 60 })} />
          <NumberInput label="Request waits (hours)" min={1} max={720} value={v.request_hours} onChange={(n) => setV({ ...v, request_hours: Number(n) || 24 })} />
        </Group>
        <Textarea label="Note" autosize minRows={1} value={v.note} onChange={(e) => setV({ ...v, note: e.target.value })} />
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end"><Button variant="default" onClick={onClose}>Cancel</Button><Button onClick={() => void save()}>Add</Button></Group>
      </Stack>
    </Modal>
  )
}
