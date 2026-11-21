import { Badge, Button, Card, Drawer, Group, Select, SegmentedControl, Stack, Text } from '@mantine/core'
import { IconArrowRight, IconCheck } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { useState } from 'react'
import { api, unwrap, type OperatorMessage } from '@/api/client'
import { keys, useMe, useOperatorMessages, useSpaces, type OperatorMessageFilters } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { atLeast, roleIn } from '@/lib/access'
import { ago } from '@/lib/format'
import { notifyError } from '@/lib/notify'
import { useView } from '@/lib/viewPrefs'
import { MessageDetail, party, stateColor } from './MessageDetail'

const kinds = ['message', 'request', 'reply', 'notification']

/** Messages addressed to operators from every readable space (SM-COMM-001). Acting on one uses the per-space operations. */
export function OperatorMessages() {
  const [filters, setFilters] = useView<OperatorMessageFilters & Record<string, unknown>>('human.messages', { state: 'open', space: '', kind: '' })
  const list = useOperatorMessages(filters)
  const spaces = useSpaces()
  const me = useMe().data
  const qc = useQueryClient()
  /** The opened message is kept as an object: acknowledging it drops it from the Unread list but the drawer stays open. */
  const [opened, setOpened] = useState<OperatorMessage | null>(null)
  const items = list.data?.pages.flatMap((p) => p.items) ?? []
  // The ack/resolve response updates `opened` at once; a list row only wins when it is newer (the refetch landed).
  const listed = opened ? items.find((m) => m.id === opened.id) : undefined
  const current = opened ? (listed && listed.version >= opened.version ? listed : opened) : null
  const canWrite = (m: OperatorMessage) => atLeast(roleIn(me, m.space_id), 'operator')
  const ack = async (m: OperatorMessage) => {
    try {
      unwrap(await api.POST('/spaces/{space_id}/messages/{message_id}/ack', { params: { path: { space_id: m.space_id, message_id: m.id } } }))
      void qc.invalidateQueries({ queryKey: keys.messages(m.space_id) })
      void qc.invalidateQueries({ queryKey: keys.operatorMessages })
    } catch (e) {
      notifyError(e, 'Could not acknowledge')
    }
  }
  return (
    <>
      <ErrorAlert error={list.error} />
      <Group mb="sm" gap="sm" align="flex-end">
        <SegmentedControl size="xs" aria-label="Which messages" value={filters.state} onChange={(v) => setFilters({ state: v as 'open' | 'all' })}
          data={[{ value: 'open', label: 'Unread' }, { value: 'all', label: 'All' }]} />
        <Select size="xs" aria-label="Space" placeholder="All spaces" clearable value={filters.space || null} onChange={(v) => setFilters({ space: v ?? '' })}
          data={(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))} />
        <Select size="xs" aria-label="Kind" placeholder="Any kind" clearable value={filters.kind || null} onChange={(v) => setFilters({ kind: v ?? '' })} data={kinds} />
      </Group>
      <Stack gap={4} role="list" aria-label="Messages to operators">
        {items.map((m) => (
          <Card key={m.id} p="xs" role="listitem" aria-label={`${m.subject}, ${m.state}`}>
            <Group gap="xs">
              <Text size="xs" fw={600} component="span">
                <Link to="/spaces/$spaceId/messages" params={{ spaceId: m.space_id }}>{m.space_name}</Link>
              </Text>
              <Badge variant="light" color={m.kind === 'request' ? 'owlYellow' : m.kind === 'notification' ? 'gray' : 'owlBlue'}>{m.kind}</Badge>
              <Text size="xs" c="dimmed" truncate>{party(m.from_kind, m.from_id, m.from_label)} <IconArrowRight size={10} /> operators</Text>
              <Text size="sm" component="button" type="button" ta="left" truncate onClick={() => setOpened(m)}
                style={{ flex: '1 1 200px', minWidth: 0, cursor: 'pointer', background: 'none', border: 0, padding: 0, color: 'inherit', font: 'inherit' }}>{m.subject}</Text>
              <Text size="xs" c="dimmed" w={70} ta="right">{ago(m.created_at)}</Text>
              <Badge color={stateColor[m.state]} variant="light">{m.state}</Badge>
              {canWrite(m) && m.state !== 'acknowledged' && m.state !== 'resolved' && (
                <Button size="compact-xs" variant="light" leftSection={<IconCheck size={12} />} aria-label={`Acknowledge ${m.subject}`} onClick={() => void ack(m)}>Acknowledge</Button>
              )}
            </Group>
          </Card>
        ))}
        {list.isSuccess && items.length === 0 && (
          <Text size="sm" c="dimmed" ta="center" p="lg">{filters.state === 'open' && !filters.space && !filters.kind ? 'No unread messages.' : 'No messages match the filters.'}</Text>
        )}
        {list.hasNextPage && <Button variant="default" size="xs" loading={list.isFetchingNextPage} onClick={() => void list.fetchNextPage()}>Load more</Button>}
      </Stack>
      <Drawer opened={current !== null} onClose={() => setOpened(null)} position="right" size="lg" title="Message">
        {current && <MessageDetail spaceId={current.space_id} m={current} canWrite={canWrite(current)} onChange={(u) => setOpened((o) => (o && o.id === u.id ? { ...o, ...u } : o))} />}
      </Drawer>
    </>
  )
}
