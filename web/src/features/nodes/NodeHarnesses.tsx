import { Badge, Card, Group, Stack, Table, Text, Tooltip } from '@mantine/core'
import type { ReactNode } from 'react'
import type { Node } from '@/api/client'
import { ago, bytes, datetime, isSet } from '@/lib/format'

type HarnessInfo = Node['capabilities']['harnesses'][number]

// A connected node re-reports its capabilities at least every 30 minutes (protocol 1.11, ADR-0077); a report older than
// this is stale.
export const STALE_AFTER_MS = 60 * 60 * 1000

/** Whether the node's last capability report is older than STALE_AFTER_MS (a node that never reported is not "stale"). */
export function capabilitiesStale(n: Pick<Node, 'capabilities_updated_at'>, now = Date.now()): boolean {
  if (!isSet(n.capabilities_updated_at)) return false
  const t = Date.parse(n.capabilities_updated_at)
  return !Number.isNaN(t) && now - t > STALE_AFTER_MS
}

/** Every harness the node reported: managed ones with their channel, channel targets and cache; others as detected. */
export function NodeHarnesses({ node, now = Date.now() }: { node: Node; now?: number }) {
  const stale = capabilitiesStale(node, now)
  return (
    <Stack gap="sm">
      <Group gap="xs">
        <Text size="sm" c="dimmed">Capabilities reported</Text>
        <Tooltip label={datetime(node.capabilities_updated_at)}>
          <Text size="sm">{isSet(node.capabilities_updated_at) ? ago(node.capabilities_updated_at, now) : 'never'}</Text>
        </Tooltip>
        {stale && (
          <Tooltip label={node.connected
            ? 'A connected node reports at least every 30 minutes: this node may run an agent older than protocol 1.11.'
            : 'The node is offline: these are the versions it reported last.'}>
            <Badge color="owlYellow" variant="light">stale</Badge>
          </Tooltip>
        )}
      </Group>
      {node.capabilities.harnesses.length === 0 && <Text size="sm" c="dimmed">No harnesses reported.</Text>}
      {node.capabilities.harnesses.map((h) => <HarnessCard key={h.kind} h={h} now={now} />)}
    </Stack>
  )
}

function HarnessCard({ h, now }: { h: HarnessInfo; now: number }) {
  const rows: [string, ReactNode][] = []
  if (h.managed) {
    rows.push(['Channel', <Text key="c" size="sm" className="mono">{h.channel || '—'}</Text>])
    rows.push(['New runs get', <Text key="v" size="sm" className="mono">{h.version || 'nothing cached yet'}</Text>])
    for (const ch of ['latest', 'stable']) {
      const r = h.resolved?.[ch]
      rows.push([`Resolved ${ch}`, r ? (
        <Group key={ch} gap={6}>
          <Text size="sm" className="mono">{r.version}</Text>
          {isSet(r.resolved_at) && <Tooltip label={datetime(r.resolved_at)}><Text size="xs" c="dimmed">{ago(r.resolved_at, now)}</Text></Tooltip>}
        </Group>
      ) : <Text key={ch} size="sm" c="dimmed">not resolved</Text>])
    }
    rows.push(['Cached', (h.cached ?? []).length > 0 ? (
      <Group key="cached" gap={4}>{(h.cached ?? []).map((v) => <Badge key={v} variant="outline" color="gray" className="mono">{v}</Badge>)}</Group>
    ) : <Text key="cached" size="sm" c="dimmed">none</Text>])
    rows.push(['Cache size', <Text key="size" size="sm">{bytes(h.cache_bytes ?? 0)}</Text>])
  } else {
    rows.push(['Version', <Text key="v" size="sm" className="mono">{h.version || '—'}</Text>])
    rows.push(['Path', <Text key="p" size="sm" className="mono" style={{ wordBreak: 'break-all' }}>{h.path || '—'}</Text>])
  }
  if (h.error) rows.push(['Error', <Text key="e" size="sm" c="owlRed">{h.error}</Text>])
  if (h.capabilities.length > 0) rows.push(['Capabilities', <Text key="caps" size="xs" c="dimmed">{h.capabilities.join(', ')}</Text>])
  return (
    <Card withBorder padding="sm" aria-label={`Harness ${h.kind}`}>
      <Group gap="xs" mb="xs">
        <Text fw={500}>{h.kind}</Text>
        <Badge color={h.available ? 'owlBlue' : 'gray'} variant={h.available ? 'light' : 'outline'}>{h.available ? 'available' : 'not available'}</Badge>
        <Badge color="gray" variant="outline">{h.managed ? 'managed by the node' : 'detected'}</Badge>
      </Group>
      <Table withRowBorders={false} verticalSpacing={2}>
        <Table.Tbody>
          {rows.map(([k, v]) => (
            <Table.Tr key={k}>
              <Table.Th w={140} style={{ verticalAlign: 'top' }}><Text size="sm" c="dimmed">{k}</Text></Table.Th>
              <Table.Td>{v}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Card>
  )
}
