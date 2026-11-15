import { Badge, Button, Card, Code, Group, Stack, Table, Text } from '@mantine/core'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, unwrap, type Approval, type HumanAction } from '@/api/client'
import { useMe } from '@/api/queries'
import { atLeast, roleIn } from '@/lib/access'
import { datetime } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'

/** The enforced approval behind an approval action, or null when the approval is advisory (no policy created it). */
export function useActionApproval(action: HumanAction) {
  return useQuery({
    queryKey: ['approval', action.space_id, action.id, action.version],
    enabled: action.type === 'approval',
    queryFn: async ({ signal }): Promise<Approval | null> => {
      const r = await api.GET('/spaces/{space_id}/human-actions/{action_id}/approval', { params: { path: { space_id: action.space_id, action_id: action.id } }, signal })
      if (r.response.status === 404) return null
      return unwrap(r)
    },
  })
}

const stateColor: Record<string, string> = { pending: 'owlYellow', approved: 'owlGreen', denied: 'owlRed', expired: 'gray', revoked: 'gray' }
const execColor: Record<string, string> = { executing: 'owlBlue', succeeded: 'owlGreen', failed: 'owlRed', interrupted: 'owlYellow' }

/**
 * What SpaceMan enforces (SM-APPR-002/004): the bound operation, arguments and limits, the decision, and each execution
 * recorded separately from it.
 */
export function EnforcedApprovalCard({ approval }: { approval: Approval }) {
  const qc = useQueryClient()
  const canRevoke = atLeast(roleIn(useMe().data, approval.space_id), 'operator')
  const revoke = async () => {
    try {
      unwrap(await api.POST('/spaces/{space_id}/approvals/{approval_id}/revoke', { params: { path: { space_id: approval.space_id, approval_id: approval.id } }, body: {} }))
      void qc.invalidateQueries({ queryKey: ['approval'] })
      notifyOk('Approval revoked')
    } catch (e) {
      notifyError(e, 'Could not revoke the approval')
    }
  }
  return (
    <Card withBorder>
      <Group justify="space-between" mb={4}>
        <Text fw={600}>Enforced approval</Text>
        <Group gap="xs">
          <Badge color={stateColor[approval.state]} variant="light">Approval: {approval.state}</Badge>
          <Badge variant="outline">{approval.uses_left} of {approval.uses_allowed} uses left</Badge>
        </Group>
      </Group>
      <Stack gap={4}>
        <Text size="sm">Operation <Code>{approval.operation}</Code> on <Code>{approval.resource}</Code>, requested by run <Code>{approval.requester_run}</Code> ({approval.requester_role})</Text>
        <Text size="xs" c="dimmed">Argument digest <span className="mono">{approval.args_digest}</span></Text>
        <Code block>{approval.args_preview}</Code>
        <Text size="xs" c="dimmed">
          {approval.state === 'pending' && `Waiting for a decision until ${datetime(approval.request_expires_at)}.`}
          {approval.valid_until && `Usable until ${datetime(approval.valid_until)}, only by this run and only with these exact arguments.`}
          {approval.state_reason && ` ${approval.state_reason}.`}
        </Text>
        <Text size="sm" fw={500} mt="xs">Executions</Text>
        {approval.executions.length === 0 ? <Text size="sm" c="dimmed">Not executed.</Text> : (
          <Table>
            <Table.Tbody>
              {approval.executions.map((x) => (
                <Table.Tr key={x.id}>
                  <Table.Td><Badge color={execColor[x.state]} variant="light">{x.state}</Badge></Table.Td>
                  <Table.Td><Text size="xs">{datetime(x.started_at)}{x.finished_at && ` → ${datetime(x.finished_at)}`}</Text></Table.Td>
                  <Table.Td><Text size="xs" c="dimmed">{x.error}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
        {canRevoke && approval.state === 'approved' && approval.uses_left > 0 && (
          <Group justify="flex-end"><Button size="xs" variant="light" color="owlRed" onClick={() => void revoke()}>Revoke before use</Button></Group>
        )}
      </Stack>
    </Card>
  )
}
