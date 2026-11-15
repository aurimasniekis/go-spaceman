import { Box, Button, Card, Group, SegmentedControl, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconPaperclip, IconPlus } from '@tabler/icons-react'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { useInbox, type InboxStatus } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { SectionHeader } from '@/features/spaces/SpaceHeader'
import { InboxStatusBadge, sourceLabel, statusLabel, UrgencyBadge } from '@/features/inbox/badges'
import { NewItemModal } from '@/features/inbox/NewItemModal'
import { useCanCreateInboxItem } from '@/features/inbox/useNewInboxItem'
import { ago, datetime } from '@/lib/format'

export const Route = createFileRoute('/spaces/$spaceId/inbox/')({ component: InboxPage })

const FILTERS: (InboxStatus | '')[] = ['new', 'in_progress', 'deferred', 'done', 'rejected', '']

function InboxPage() {
  const { spaceId } = Route.useParams()
  const navigate = useNavigate()
  const [status, setStatus] = useState<InboxStatus | ''>('new')
  const [creating, setCreating] = useState(false)
  const inbox = useInbox(spaceId, status)
  const counts = inbox.data?.pages[0]?.counts
  const items = inbox.data?.pages.flatMap((p) => p.items) ?? []
  const canWrite = useCanCreateInboxItem(spaceId)

  return (
    <>
      <SectionHeader title="Inbox" description="Requests, events and messages for control to triage"
        actions={canWrite && <Button leftSection={<IconPlus size={14} />} onClick={() => setCreating(true)}>New item</Button>}
      />
      <Stack gap="sm">
        {/* Six filters are wider than a phone: they scroll in their own row instead of widening the page. */}
        <Box style={{ overflowX: 'auto', maxWidth: '100%', scrollbarWidth: 'thin' }}>
          <SegmentedControl
            aria-label="Filter by status"
            w="fit-content"
            value={status || 'all'}
            onChange={(v) => setStatus(v === 'all' ? '' : (v as InboxStatus))}
            data={FILTERS.map((f) => ({ value: f || 'all', label: f ? `${statusLabel[f]}${counts ? ` (${counts[f] ?? 0})` : ''}` : 'All' }))}
          />
        </Box>
        <ErrorAlert error={inbox.error} />
        <Card p={0}>
          <Table.ScrollContainer minWidth={560}>
            <Table>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Item</Table.Th>
                  <Table.Th>From</Table.Th>
                  <Table.Th>Status</Table.Th>
                  <Table.Th>Received</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {items.map((it) => (
                  <Table.Tr key={it.id} style={{ cursor: 'pointer' }} onClick={() => void navigate({ to: '/spaces/$spaceId/inbox/$itemId', params: { spaceId, itemId: it.id } })}>
                    <Table.Td>
                      <Group gap={6} wrap="nowrap">
                        <UrgencyBadge urgency={it.urgency} />
                        <AnchorLink to="/spaces/$spaceId/inbox/$itemId" params={{ spaceId, itemId: it.id }} size="sm" fw={it.status === 'new' ? 600 : 400}
                          onClick={(e) => e.stopPropagation()}>{it.title}</AnchorLink>
                        {it.content_type === 'application/json' && <Text size="xs" c="dimmed" className="mono">JSON</Text>}
                        {it.body.includes('spaceman://attachment/') && <IconPaperclip size={12} aria-label="Has attachments" />}
                      </Group>
                    </Table.Td>
                    <Table.Td><Text size="xs">{it.source_label || sourceLabel[it.source_kind]}</Text><Text size="xs" c="dimmed">{sourceLabel[it.source_kind]}</Text></Table.Td>
                    <Table.Td><InboxStatusBadge status={it.status} /></Table.Td>
                    <Table.Td><Tooltip label={datetime(it.received_at)}><Text size="xs" c="dimmed">{ago(it.received_at)}</Text></Tooltip></Table.Td>
                  </Table.Tr>
                ))}
                {inbox.isSuccess && items.length === 0 && (
                  <Table.Tr><Table.Td colSpan={4}><Text size="sm" c="dimmed" ta="center" p="md">{status ? `No ${statusLabel[status].toLowerCase()} items.` : 'The inbox is empty.'}</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
        {inbox.hasNextPage && <Button variant="default" w="fit-content" loading={inbox.isFetchingNextPage} onClick={() => void inbox.fetchNextPage()}>Load older</Button>}
      </Stack>
      <NewItemModal spaceId={spaceId} opened={creating} onClose={() => setCreating(false)}
        onCreated={(it) => void navigate({ to: '/spaces/$spaceId/inbox/$itemId', params: { spaceId, itemId: it.id } })} />
    </>
  )
}
