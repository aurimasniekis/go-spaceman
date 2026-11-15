import { Alert, Badge, Button, Card, Group, Stack, Table, Tabs, Text, Title, Tooltip } from '@mantine/core'
import { IconTerminal2 } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useMe, useRuns, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { RunState } from '@/components/StatusBadge'
import { SpaceAccountsCard } from '@/features/accounts/SpaceAccountsCard'
import { SpaceAttentionCard } from '@/features/attention/SpaceAttentionCard'
import { SpaceRolesCard } from '@/features/catalog/SpaceRolesCard'
import { SpaceHarborPilotCard } from '@/features/harborpilot/SpaceHarborPilotCard'
import { SpaceMcpCard } from '@/features/mcp/SpaceMcpCard'
import { RunLimitBadge } from '@/features/limits/LimitTime'
import { harnessLabel, isEnded, kindLabel, RunDot, runStateText, runTitle, sortRuns } from '@/features/runs/runInfo'
import { RelocationCard } from '@/features/spaces/RelocationCard'
import { SpaceUsageCard } from '@/features/usage/UsageCards'
import { hasGlobal } from '@/lib/access'
import { ago, datetime, isSet } from '@/lib/format'

export const Route = createFileRoute('/spaces/$spaceId/')({ component: SpacePage })

/**
 * A space's overview (UX review §3.2): what needs attention, the agents running now, then configuration and usage, and
 * the run history last. The header and its actions are the space layout's.
 */
function SpacePage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const runs = useRuns(spaceId)
  const me = useMe().data
  const canMove = hasGlobal(me, 'admin')
  if (space.error) return <ErrorAlert error={space.error} />
  if (!space.data) return null
  const s = space.data
  const all = sortRuns(runs.data ?? [])
  const live = all.filter((r) => !isEnded(r))
  return (
    <Stack gap="md">
      {s.lease && !s.lease.valid && (
        <Alert color="owlRed" title="No valid execution lease">
          The node holds no valid lease for this space ({s.lease.state}), so no new work starts until it reconnects.
        </Alert>
      )}
      <SpaceAttentionCard spaceId={s.id} />
      <RelocationCard spaceId={s.id} canMove={canMove} />

      <Card>
        <Group justify="space-between" mb="xs">
          <Group gap="xs">
            <Title order={5}>Agents</Title>
            <Text size="sm" c="dimmed">{live.length === 0 ? 'none running' : `${live.length} running`}</Text>
          </Group>
          <Button variant="default" size="xs" leftSection={<IconTerminal2 size={14} />}
            renderRoot={(props) => <AnchorLink {...props} to="/spaces/$spaceId/terminals" params={{ spaceId }} underline="never" />}>
            Open terminals
          </Button>
        </Group>
        {live.length === 0 ? (
          <Text size="sm" c="dimmed">No agent is running. Control coordinates the space: launch it first.</Text>
        ) : (
          <Table>
            <Table.Thead>
              <Table.Tr><Table.Th>Agent</Table.Th><Table.Th>Kind</Table.Th><Table.Th>State</Table.Th><Table.Th>Started</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {live.map((r) => (
                <Table.Tr key={r.id}>
                  <Table.Td>
                    <Group gap={8} wrap="nowrap">
                      <RunDot run={r} />
                      <AnchorLink to="/spaces/$spaceId/terminals" params={{ spaceId }} search={{ run: r.id }} size="sm" fw={500}>{runTitle(r)}</AnchorLink>
                    </Group>
                  </Table.Td>
                  <Table.Td><Text size="sm" c="dimmed">{kindLabel(r.kind)} · {harnessLabel(r.harness)}</Text></Table.Td>
                  <Table.Td>
                    <Group gap={6} wrap="wrap"><Text size="sm">{runStateText(r)}</Text><RunLimitBadge run={r} /></Group>
                  </Table.Td>
                  <Table.Td><Tooltip label={datetime(r.started_at ?? r.created_at)}><Text size="sm" c="dimmed">{ago(isSet(r.started_at) ? r.started_at : r.created_at)}</Text></Tooltip></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Card>

      <SpaceRolesCard spaceId={s.id} />
      <SpaceMcpCard spaceId={s.id} />
      <SpaceAccountsCard spaceId={s.id} />
      <SpaceHarborPilotCard spaceId={s.id} />
      <SpaceUsageCard spaceId={s.id} />

      <Tabs defaultValue="runs" keepMounted={false}>
        <Tabs.List>
          <Tabs.Tab value="runs">Run history</Tabs.Tab>
          <Tabs.Tab value="about">Goals &amp; instructions</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="runs" pt="sm">
          <ErrorAlert error={runs.error} />
          <Card p={0}>
            <Table>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Agent</Table.Th>
                  <Table.Th>Kind</Table.Th>
                  <Table.Th>State</Table.Th>
                  <Table.Th>Started</Table.Th>
                  <Table.Th>Run</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {all.map((r) => (
                  <Table.Tr key={r.id}>
                    <Table.Td>
                      <Group gap={8} wrap="nowrap">
                        <RunDot run={r} />
                        <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: r.id }} size="sm">{runTitle(r)}</AnchorLink>
                      </Group>
                    </Table.Td>
                    <Table.Td><Text size="sm" c="dimmed">{kindLabel(r.kind)} · {harnessLabel(r.harness)}</Text></Table.Td>
                    <Table.Td><RunState run={r} /></Table.Td>
                    <Table.Td><Tooltip label={datetime(r.started_at ?? r.created_at)}><Text size="sm" c="dimmed">{ago(isSet(r.started_at) ? r.started_at : r.created_at)}</Text></Tooltip></Table.Td>
                    <Table.Td><Text size="xs" c="dimmed" className="mono">{r.id}</Text></Table.Td>
                  </Table.Tr>
                ))}
                {runs.isSuccess && all.length === 0 && (
                  <Table.Tr><Table.Td colSpan={5}><Text size="sm" c="dimmed" ta="center" p="md">No runs yet.</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Card>
        </Tabs.Panel>
        <Tabs.Panel value="about" pt="sm">
          <Stack gap="md">
            <div>
              <Title order={5}>Goals</Title>
              <Text size="sm" style={{ whiteSpace: 'pre-wrap' }} c={s.goals ? undefined : 'dimmed'}>{s.goals || 'None set.'}</Text>
            </div>
            <div>
              <Title order={5}>Standing instructions</Title>
              <Text size="sm" style={{ whiteSpace: 'pre-wrap' }} c={s.standing_instructions ? undefined : 'dimmed'}>{s.standing_instructions || 'None set.'}</Text>
            </div>
            <div>
              <Title order={5}>Working directory</Title>
              <Text size="sm" className="mono">{s.workdir || '—'}</Text>
              <Text size="xs" c="dimmed">If the node disconnects for longer than the grace period, its runs are {s.disconnect_policy === 'kill' ? 'stopped' : 'interrupted'}.</Text>
            </div>
            {s.description && (
              <div>
                <Title order={5}>Description</Title>
                <Text size="sm">{s.description}</Text>
              </div>
            )}
            <Group gap="xs"><Badge variant="outline" color="gray">lease generation {s.lease?.generation ?? '—'}</Badge></Group>
          </Stack>
        </Tabs.Panel>
      </Tabs>
    </Stack>
  )
}
