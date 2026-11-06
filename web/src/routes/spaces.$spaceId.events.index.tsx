import { Badge, Button, Card, Group, Table, Text, Tooltip } from '@mantine/core'
import { IconPlus } from '@tabler/icons-react'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { useMe, useSubscriptions } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { DeliveryBadge, SubscriptionState } from '@/features/events/badges'
import { SubscriptionModal } from '@/features/events/SubscriptionModal'
import { atLeast, roleIn } from '@/lib/access'
import { ago, datetime } from '@/lib/format'

export const Route = createFileRoute('/spaces/$spaceId/events/')({ component: SpaceEventsPage })

function SpaceEventsPage() {
  const { spaceId } = Route.useParams()
  const subs = useSubscriptions(spaceId)
  const me = useMe().data
  const [creating, setCreating] = useState(false)
  return (
    <>
      <SectionHeader title="Events" description="Subscriptions that deliver matching events into this space's inbox"
        actions={atLeast(roleIn(me, spaceId), 'operator') && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>Subscribe</Button>} />
      <ErrorAlert error={subs.error} />
      <Card p={0}>
        <Table>
          <Table.Thead>
            <Table.Tr><Table.Th>Subscription</Table.Th><Table.Th>Template</Table.Th><Table.Th>Subject</Table.Th><Table.Th>State</Table.Th><Table.Th>Last event</Table.Th><Table.Th>Quarantine</Table.Th></Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {(subs.data ?? []).map((s) => (
              <Table.Tr key={s.id}>
                <Table.Td><AnchorLink to="/spaces/$spaceId/events/$subscriptionId" params={{ spaceId, subscriptionId: s.id }} size="sm" fw={500}>{s.name}</AnchorLink></Table.Td>
                <Table.Td><Text size="xs">{s.template ? `${s.template.template_slug} v${s.template.version}${s.follow === 'latest' ? ' · following' : ''}` : 'custom'}</Text></Table.Td>
                <Table.Td><Text size="xs" className="mono">{s.subject}</Text></Table.Td>
                <Table.Td><Group gap={4} wrap="nowrap"><SubscriptionState sub={s} /><DeliveryBadge sub={s} /></Group></Table.Td>
                <Table.Td><Tooltip label={s.last_event_at ? datetime(s.last_event_at) : 'none yet'}><Text size="xs" c="dimmed">{s.last_event_at ? ago(s.last_event_at) : '—'}</Text></Tooltip></Table.Td>
                <Table.Td>{s.quarantined > 0 ? <Badge color="owlRed">{s.quarantined} open</Badge> : <Text size="xs" c="dimmed">—</Text>}</Table.Td>
              </Table.Tr>
            ))}
            {subs.isSuccess && subs.data.length === 0 && <Table.Tr><Table.Td colSpan={6}><Text size="sm" c="dimmed" ta="center" p="md">No subscriptions. Subscribe to bring events into this space's inbox.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>
      </Card>
      <SubscriptionModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)} />
    </>
  )
}
