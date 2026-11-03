import { Card, Group, Text } from '@mantine/core'
import { useAttention } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { SpaceCounts } from './AttentionView'

/** A space's attention counters (SM-UI-003): what in this space needs a person, each linked to where it is handled. */
export function SpaceAttentionCard({ spaceId }: { spaceId: string }) {
  const q = useAttention(spaceId)
  const s = q.data?.spaces[0]
  if (!s) return null
  return (
    <Card mb="md" aria-label="Needs attention in this space">
      <Group justify="space-between" mb={4}>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Needs attention</Text>
        <AnchorLink to="/attention" size="xs">All spaces</AnchorLink>
      </Group>
      {s.total === 0 ? <Text size="sm" c="dimmed">Nothing in this space needs a person.</Text> : <SpaceCounts s={s} />}
    </Card>
  )
}
