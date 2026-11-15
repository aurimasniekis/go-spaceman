import { Card, Group, Stack, Text } from '@mantine/core'
import { useRuns } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { RunState } from '@/components/StatusBadge'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { harnessLabel, kindLabel, RunDot, runStateText, runTitle, sortRuns } from '@/features/runs/runInfo'
import { ago, isSet } from '@/lib/format'

/** A space's runs, read-only (no terminal in the Mini App yet): live first, then the history. */
export function TgRuns({ spaceId }: { spaceId: string }) {
  const runs = useRuns(spaceId)
  const all = sortRuns(runs.data ?? [])
  return (
    <>
      <SectionHeader title="Runs" description="Agents of this space and their history" />
      <ErrorAlert error={runs.error} />
      <Stack gap={4} role="list" aria-label="Runs">
        {all.map((r) => (
          <Card key={r.id} p="xs" role="listitem">
            <Group gap="xs" wrap="nowrap" justify="space-between" align="flex-start">
              <Stack gap={0} style={{ minWidth: 0 }}>
                <Group gap={8} wrap="nowrap"><RunDot run={r} /><AnchorLink to="/spaces/$spaceId/runs/$runId" params={{ spaceId, runId: r.id }} size="sm" fw={500}>{runTitle(r)}</AnchorLink></Group>
                <Text size="xs" c="dimmed">{kindLabel(r.kind)} · {harnessLabel(r.harness)} · {ago(isSet(r.started_at) ? r.started_at : r.created_at)}</Text>
                <Text size="xs" c="dimmed">{runStateText(r)}</Text>
              </Stack>
              <RunState run={r} />
            </Group>
          </Card>
        ))}
        {runs.isSuccess && all.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">No runs yet.</Text>}
      </Stack>
    </>
  )
}
