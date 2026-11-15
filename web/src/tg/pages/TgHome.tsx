import { Card, Group, SimpleGrid, Stack, Text } from '@mantine/core'
import { IconSettings } from '@tabler/icons-react'
import { useQueries } from '@tanstack/react-query'
import { runsQuery, useAttention, useMe, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SpaceState } from '@/components/StatusBadge'
import { isEnded, runTitle, RunDot, runStateText } from '@/features/runs/runInfo'

/**
 * The Mini App's start page: what needs you, the agents running now and the spaces. The web overview's node list and
 * usage are not reachable from a Telegram link, so this is its own page.
 */
export function TgHome() {
  const me = useMe().data
  const spaces = useSpaces()
  const attention = useAttention()
  const list = (spaces.data ?? []).filter((s) => s.state !== 'archived').slice(0, 50)
  const runs = useQueries({ queries: list.map((s) => runsQuery(s.id)) })
  const active = runs.flatMap((q) => q.data ?? []).filter((r) => !isEnded(r))
  const name = (id: string) => list.find((s) => s.id === id)?.name ?? id
  return (
    <Stack gap="md">
      <PageHeader title="Overview" description={me?.user ? `Welcome back, ${me.user.display_name}.` : undefined}
        actions={<AnchorLink to="/settings" aria-label="Telegram settings"><IconSettings size={20} /></AnchorLink>} />
      <ErrorAlert error={spaces.error} />
      <SimpleGrid cols={2}>
        <Card><Text size="xs" c="dimmed" tt="uppercase" fw={600}>Spaces</Text><Text fw={600} size="xl">{list.length}</Text></Card>
        <Card>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Needs attention</Text>
          <Text fw={600} size="xl">{attention.data?.total ?? '—'}</Text>
          <AnchorLink to="/attention" size="xs">Open</AnchorLink>
        </Card>
      </SimpleGrid>
      <Stack gap="xs">
        <Text fw={600}>Active runs</Text>
        {active.length === 0 && <Text size="sm" c="dimmed">No runs are active.</Text>}
        {active.slice(0, 20).map((r) => (
          <Card key={r.id} p="xs">
            <Group gap="xs" wrap="nowrap" justify="space-between">
              <Stack gap={0} style={{ minWidth: 0 }}>
                <Group gap={8} wrap="nowrap"><RunDot run={r} /><AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId: r.space_id, runId: r.id }} size="sm" fw={500}>{runTitle(r)}</AnchorLink></Group>
                <Text size="xs" c="dimmed" truncate>{name(r.space_id)}</Text>
              </Stack>
              <Text size="xs" c="dimmed">{runStateText(r)}</Text>
            </Group>
          </Card>
        ))}
      </Stack>
      <Stack gap="xs">
        <Text fw={600}>Spaces</Text>
        {spaces.isSuccess && list.length === 0 && <Text size="sm" c="dimmed">No spaces are shared with you.</Text>}
        {list.map((s) => (
          <Card key={s.id} p="xs">
            <Group justify="space-between" wrap="nowrap">
              <AnchorLink to="/spaces/$spaceId" params={{ spaceId: s.id }} size="sm" fw={500}>{s.name}</AnchorLink>
              <SpaceState space={s} />
            </Group>
          </Card>
        ))}
      </Stack>
    </Stack>
  )
}
