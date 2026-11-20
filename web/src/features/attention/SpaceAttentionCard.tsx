import { Card, Group, Text } from '@mantine/core'
import { useAttention } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { SpaceCounts, spaceHasItems } from './AttentionView'

/**
 * A space's attention counters (SM-UI-003): what in this space needs a person, each linked to where it is handled.
 * In the Mini App there is no all-spaces page, and the web-only pages get no links.
 */
export function SpaceAttentionCard({ spaceId, miniApp = false }: { spaceId: string; miniApp?: boolean }) {
  const q = useAttention(spaceId)
  const s = q.data?.spaces[0]
  if (!s) return null
  return (
    <Card mb="md" aria-label="Needs attention in this space">
      <Group justify="space-between" mb={4}>
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Needs attention</Text>
        {!miniApp && <AnchorLink to="/attention" size="xs">All spaces</AnchorLink>}
      </Group>
      {!spaceHasItems(s) ? <Text size="sm" c="dimmed">Nothing in this space needs a person.</Text> : <SpaceCounts s={s} miniApp={miniApp} />}
    </Card>
  )
}
