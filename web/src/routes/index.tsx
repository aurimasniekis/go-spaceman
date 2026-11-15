import { Card, Group, SimpleGrid, Stack, Table, Text } from '@mantine/core'
import { useQueries } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import { runsQuery, useAttention, useMe, useNodes, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { NodeState, RunState, SpaceState } from '@/components/StatusBadge'
import { hasGlobal } from '@/lib/access'
import { ago } from '@/lib/format'

export const Route = createFileRoute('/')({ component: Overview })

const ENDED = ['exited', 'failed', 'lost']

function Stat({ label, value, hint }: { label: string; value: number | string; hint?: ReactNode }) {
  return (
    <Card>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text fw={600} size="xl" mt={2}>{value}</Text>
      {hint && <Text size="xs" c="dimmed">{hint}</Text>}
    </Card>
  )
}

function Overview() {
  const me = useMe().data
  const spaces = useSpaces()
  const nodes = useNodes()
  const showNodes = hasGlobal(me)
  const list = (spaces.data ?? []).filter((s) => s.state !== 'archived').slice(0, 50)
  // The same query as the space and Terminals pages (one cached list per space, one request for all of them).
  const runs = useQueries({ queries: list.map((s) => runsQuery(s.id)) })
  const active = runs.flatMap((q) => q.data ?? []).filter((r) => !ENDED.includes(r.observed))
  const name = (id: string) => list.find((s) => s.id === id)?.name ?? id
  const connected = (nodes.data ?? []).filter((n) => n.connected).length
  const attention = useAttention()

  return (
    <>
      <PageHeader title="Overview" description={me?.user ? `Welcome back, ${me.user.display_name}.` : undefined} />
      <ErrorAlert error={spaces.error} />
      <SimpleGrid cols={{ base: 2, sm: 4 }} mb="md">
        <Stat label="Spaces" value={list.length} hint={`${list.filter((s) => s.state === 'paused').length} paused`} />
        <Stat label="Active runs" value={active.length} hint={`${active.filter((r) => r.status === 'working').length} working`} />
        <Stat label="Needs attention" value={attention.data?.total ?? '—'}
          hint={<AnchorLink to="/attention" size="xs">human actions, tasks, runs, storage…</AnchorLink>} />
        {showNodes && <Stat label="Nodes" value={`${connected}/${nodes.data?.length ?? 0}`} hint="connected" />}
      </SimpleGrid>
      <SimpleGrid cols={{ base: 1, md: showNodes ? 2 : 1 }}>
        <Stack gap="xs">
          <Text fw={600}>Active runs</Text>
          <Card p={0}>
            <Table>
              <Table.Tbody>
                {active.slice(0, 20).map((r) => (
                  <Table.Tr key={r.id}>
                    <Table.Td>
                      <AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId: r.space_id, runId: r.id }} size="sm">{r.role}</AnchorLink>
                      <Text size="xs" c="dimmed">{name(r.space_id)} · {r.harness}</Text>
                    </Table.Td>
                    <Table.Td><RunState run={r} /></Table.Td>
                    <Table.Td><Text size="xs" c="dimmed">{r.status || '—'}</Text></Table.Td>
                    <Table.Td><Text size="xs" c="dimmed">{ago(r.started_at ?? r.created_at)}</Text></Table.Td>
                  </Table.Tr>
                ))}
                {active.length === 0 && <Table.Tr><Table.Td><Text size="sm" c="dimmed" ta="center" p="md">No runs are active.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Card>
          <Text fw={600} mt="sm">Spaces</Text>
          <Card p={0}>
            <Table>
              <Table.Tbody>
                {list.map((s) => (
                  <Table.Tr key={s.id}>
                    <Table.Td><AnchorLink to="/spaces/$spaceId" params={{ spaceId: s.id }} size="sm">{s.name}</AnchorLink></Table.Td>
                    <Table.Td><SpaceState space={s} /></Table.Td>
                  </Table.Tr>
                ))}
                {spaces.isSuccess && list.length === 0 && <Table.Tr><Table.Td><Text size="sm" c="dimmed" ta="center" p="md">No spaces yet.</Text></Table.Td></Table.Tr>}
              </Table.Tbody>
            </Table>
          </Card>
        </Stack>
        {showNodes && (
          <Stack gap="xs">
            <Text fw={600}>Nodes</Text>
            <Card p={0}>
              <Table>
                <Table.Tbody>
                  {(nodes.data ?? []).map((n) => (
                    <Table.Tr key={n.id}>
                      <Table.Td><Text size="sm">{n.name}</Text><Text size="xs" c="dimmed">{n.agent_version || 'not enrolled'}</Text></Table.Td>
                      <Table.Td><NodeState node={n} /></Table.Td>
                      <Table.Td>
                        <Group gap={4}>{n.capabilities.harnesses.filter((h) => h.available).map((h) => <Text key={h.kind} size="xs" c="dimmed">{h.kind}</Text>)}</Group>
                      </Table.Td>
                    </Table.Tr>
                  ))}
                </Table.Tbody>
              </Table>
            </Card>
          </Stack>
        )}
      </SimpleGrid>
    </>
  )
}
