import { Alert, Card, Group, Stack, Text, Title } from '@mantine/core'
import { useRuns, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { SpaceAttentionCard } from '@/features/attention/SpaceAttentionCard'
import { isEnded, RunDot, runStateText, runTitle, sortRuns } from '@/features/runs/runInfo'

/** A space's overview in the Mini App: what needs attention, the agents running now and the space's goals. */
export function TgSpaceOverview({ spaceId }: { spaceId: string }) {
  const space = useSpace(spaceId)
  const runs = useRuns(spaceId)
  if (space.error) return <ErrorAlert error={space.error} />
  if (!space.data) return null
  const s = space.data
  const live = sortRuns(runs.data ?? []).filter((r) => !isEnded(r))
  return (
    <Stack gap="md">
      {s.lease && !s.lease.valid && (
        <Alert color="owlRed" title="No valid execution lease">The node holds no valid lease for this space ({s.lease.state}), so no new work starts until it reconnects.</Alert>
      )}
      <SpaceAttentionCard spaceId={s.id} miniApp />
      <Card>
        <Group justify="space-between" mb="xs">
          <Title order={5}>Agents</Title>
          <Text size="sm" c="dimmed">{live.length === 0 ? 'none running' : `${live.length} running`}</Text>
        </Group>
        <Stack gap="xs">
          {live.map((r) => (
            <Group key={r.id} gap="xs" justify="space-between" wrap="nowrap">
              <Group gap={8} wrap="nowrap"><RunDot run={r} /><AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: r.id }} size="sm" fw={500}>{runTitle(r)}</AnchorLink></Group>
              <Text size="xs" c="dimmed">{runStateText(r)}</Text>
            </Group>
          ))}
        </Stack>
      </Card>
      {(s.goals || s.description) && (
        <Card>
          {s.description && <Text size="sm" mb="xs">{s.description}</Text>}
          {s.goals && <><Title order={5}>Goals</Title><Text size="sm" style={{ whiteSpace: 'pre-wrap' }}>{s.goals}</Text></>}
        </Card>
      )}
    </Stack>
  )
}
