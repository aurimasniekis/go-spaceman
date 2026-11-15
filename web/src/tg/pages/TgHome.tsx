import { Button, Card, Group, Stack, Text, Title } from '@mantine/core'
import { IconSettings } from '@tabler/icons-react'
import { Link } from '@tanstack/react-router'
import { useQueries } from '@tanstack/react-query'
import { runsQuery, useAttention, useHumanPending, useMe, useSpaces } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { SpaceState } from '@/components/StatusBadge'
import { ActionList } from '@/features/human/ActionList'
import { isEnded, runTitle, RunDot, runStateText } from '@/features/runs/runInfo'
import type { HumanAction } from '@/api/client'

/** How many pending actions the home lists; the rest are one tap away on "See all". */
const NEEDS_YOU_TOP = 5

const URGENCY: Record<HumanAction['priority'], number> = { urgent: 0, high: 1, normal: 2, low: 3 }

/** The most urgent first, then the oldest; the server's order breaks the rest. */
function topNeedsYou(items: HumanAction[], n = NEEDS_YOU_TOP): HumanAction[] {
  return items
    .map((a, i) => ({ a, i }))
    .sort((x, y) => URGENCY[x.a.priority] - URGENCY[y.a.priority] || x.a.created_at.localeCompare(y.a.created_at) || x.i - y.i)
    .slice(0, n)
    .map((x) => x.a)
}

/**
 * The Mini App's start page: what needs you across your spaces first, then the agents running now and the spaces. The
 * web overview's node list and usage are not reachable from a Telegram link, so this is its own page. The counts stay
 * live through the change feed, which refreshes the pending list (`['human', 'pending']`).
 */
export function TgHome() {
  const me = useMe().data
  const spaces = useSpaces()
  const attention = useAttention()
  const human = useHumanPending()
  const list = (spaces.data ?? []).filter((s) => s.state !== 'archived').slice(0, 50)
  const runs = useQueries({ queries: list.map((s) => runsQuery(s.id)) })
  const active = runs.flatMap((q) => q.data ?? []).filter((r) => !isEnded(r))
  const name = (id: string) => list.find((s) => s.id === id)?.name ?? id
  const names = Object.fromEntries((spaces.data ?? []).map((s) => [s.id, s.name]))
  const pending = human.data?.items ?? []
  const total = human.data?.total
  return (
    <Stack gap="md">
      <PageHeader title="Overview" description={me?.user ? `Welcome back, ${me.user.display_name}.` : undefined}
        actions={<AnchorLink to="/settings" aria-label="Telegram settings"><IconSettings size={20} /></AnchorLink>} />
      <ErrorAlert error={human.error} />
      <Stack gap="xs" component="section" aria-labelledby="needs-you-heading">
        <Group justify="space-between" wrap="nowrap">
          <Group gap="xs">
            <Title order={3} size="h4" id="needs-you-heading">Needs you</Title>
            <Text size="sm" c="dimmed" aria-live="polite">{total ?? '—'}</Text>
          </Group>
          <Button size="xs" variant="light" component={Link} to="/human">See all</Button>
        </Group>
        {human.isSuccess && pending.length === 0 && <Text size="sm" c="dimmed">Nothing is waiting for you.</Text>}
        {pending.length > 0 && (
          <ActionList actions={topNeedsYou(pending)} spaceNames={names} empty="Nothing is waiting for you." />
        )}
      </Stack>
      <Group gap="xs" justify="space-between" wrap="nowrap">
        <Text size="sm" c="dimmed">Spaces {list.length}</Text>
        <Text size="sm" c="dimmed">
          Needs attention {attention.data?.total ?? '—'}{' '}
          <AnchorLink to="/attention" size="xs">Open</AnchorLink>
        </Text>
      </Group>
      <ErrorAlert error={spaces.error} />
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
