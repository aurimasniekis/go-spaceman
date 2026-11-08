import { Badge, Button, Card, Group, Stack, Text } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { useState } from 'react'
import { useMe, useSpaceMcp } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { hasGlobal } from '@/lib/access'
import { AddSpacemanMcpModal } from './AddSpacemanMcpModal'

/** The MCP servers granted in a space, and (for administrators) "Add SpaceMan MCP" with the space prefilled (ADR-0063). */
export function SpaceMcpCard({ spaceId }: { spaceId: string }) {
  const items = useSpaceMcp(spaceId).data ?? []
  const admin = hasGlobal(useMe().data, 'admin')
  const [adding, setAdding] = useState(false)
  return (
    <Card mb="md">
      <Group justify="space-between" mb="xs" wrap="wrap" gap="xs">
        <Text size="xs" c="dimmed" tt="uppercase" fw={600}>MCP servers</Text>
        {admin && (
          <Group gap="xs">
            <Button size="xs" variant="subtle" component={AnchorLink} to="/mcp-servers">All servers</Button>
            <Button size="xs" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setAdding(true)}>Add SpaceMan MCP</Button>
          </Group>
        )}
      </Group>
      {items.length === 0 ? (
        <Text size="sm" c="dimmed">No MCP servers are granted here.{admin ? ' Add SpaceMan MCP to give this space’s control runs SpaceMan’s own tools.' : ''}</Text>
      ) : (
        <Stack gap={4}>
          {items.map((m) => (
            <Group key={m.grant_id} gap="xs" wrap="wrap">
              <Text size="sm" fw={500}>{m.name}</Text>
              {m.kind === 'spaceman' && <Badge size="xs" color="owlPurple" variant="light">SpaceMan</Badge>}
              {!m.enabled && <Badge size="xs" color="gray">disabled</Badge>}
              <Text size="xs" c="dimmed">{m.role === '*' ? 'every role' : <span className="mono">{m.role}</span>} · {m.tools.length ? `${m.tools.length} tools` : 'all offered tools'}</Text>
            </Group>
          ))}
        </Stack>
      )}
      <AddSpacemanMcpModal opened={adding} onClose={() => setAdding(false)} spaceId={spaceId} />
    </Card>
  )
}
