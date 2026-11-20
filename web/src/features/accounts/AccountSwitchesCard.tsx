import { Badge, Card, Group, Table, Text, Title } from '@mantine/core'
import { useAccountSwitches, useSpaceAccounts } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { LimitTime } from '@/features/limits/LimitTime'
import { limitKindLabel } from '@/features/limits/usageLimit'
import { isSwitchBack, reasonLabel, SWITCH_CAUSE, switchStateLabel } from './switches'

const RECENT = 20

/** Where runs moved when an account reached its usage limit (ADR-0090), newest first, with links to both runs. */
export function AccountSwitchesCard({ spaceId }: { spaceId: string }) {
  const switches = useAccountSwitches(spaceId)
  const bound = useSpaceAccounts(spaceId)
  const name = (id?: string) => (id ? (bound.data?.find((b) => b.account_id === id)?.name ?? (bound.data ? 'a removed account' : 'an account')) : 'no account')
  const items = (switches.data ?? []).slice(0, RECENT)
  return (
    <Card p={0} data-testid="account-switches">
      <Group px="md" pt="md" pb={4}><Title order={5}>Account switches</Title></Group>
      <Text size="sm" c="dimmed" px="md" pb="sm">Runs that moved to another account of their pool at a usage limit, those that waited for the reset instead, and those that switched back to their original account after its reset.</Text>
      <div style={{ padding: '0 var(--mantine-spacing-md)' }}><ErrorAlert error={switches.error} /></div>
      {items.length === 0 && !switches.isLoading ? <Text size="sm" c="dimmed" px="md" pb="md">No run has switched accounts yet.</Text> : (
        <Table.ScrollContainer minWidth={640}>
          <Table verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr><Table.Th>When</Table.Th><Table.Th>Role</Table.Th><Table.Th>Accounts</Table.Th><Table.Th>Cause</Table.Th><Table.Th>State</Table.Th><Table.Th>Runs</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {items.map((s) => {
                const st = switchStateLabel(s)
                const back = isSwitchBack(s)
                return (
                  <Table.Tr key={s.id}>
                    <Table.Td><LimitTime at={s.created_at} /></Table.Td>
                    <Table.Td><Text size="sm">{s.role}</Text></Table.Td>
                    <Table.Td><Text size="sm">{name(s.from_account_id)} → {s.to_account_id ? name(s.to_account_id) : 'none'}</Text></Table.Td>
                    <Table.Td>
                      <Text size="sm">{back ? SWITCH_CAUSE.switch_back : limitKindLabel(s.limit_kind)}</Text>
                      {back ? <Text size="xs" c="dimmed">original account reset</Text> : s.limit_resets_at && <Text size="xs" c="dimmed">resets <LimitTime at={s.limit_resets_at} /></Text>}
                    </Table.Td>
                    <Table.Td>
                      <Badge variant="light" color={st.color} style={{ textTransform: 'none', maxWidth: 'none' }}>{st.label}</Badge>
                      {s.reason && <Text size="xs" c="dimmed">because {reasonLabel(s.reason, s.cause)}</Text>}
                    </Table.Td>
                    <Table.Td>
                      <Group gap="xs" wrap="nowrap">
                        <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: s.from_run_id }} size="sm">{back ? 'previous run' : 'limited run'}</AnchorLink>
                        {s.new_run_id && <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: s.new_run_id }} size="sm">new run</AnchorLink>}
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      )}
    </Card>
  )
}
