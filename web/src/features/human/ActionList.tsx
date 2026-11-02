import { Badge, Button, Card, Checkbox, Drawer, Group, Stack, Text } from '@mantine/core'
import { IconCheck, IconHandGrab } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, ifMatch, unwrap, type HumanAction } from '@/api/client'
import { useMe } from '@/api/queries'
import { atLeast, roleIn } from '@/lib/access'
import { ago } from '@/lib/format'
import { notifyError, notifyOk } from '@/lib/notify'
import { ActionDetail } from './ActionDetail'
import { HumanStatusBadge, isPending, PriorityBadge, requesterLabel, TypeBadge, type MessageReq } from './badges'

/**
 * A list of human actions with a detail drawer. Bulk operations are limited to safe ones (SM-HUI-003): claiming, and
 * acknowledging messages. Approvals, answers and completions are always made one at a time, with the request visible.
 */
export function ActionList({ actions, spaceNames, empty }: { actions: HumanAction[]; spaceNames?: Record<string, string>; empty: string }) {
  const qc = useQueryClient()
  const me = useMe().data
  // The drawer keeps the last known copy, so an action that leaves a pending-only list (because it was just answered)
  // stays open with its recorded response.
  const [open, setOpenState] = useState<HumanAction | null>(null)
  const setOpen = (id: string | null) => setOpenState(actions.find((a) => a.id === id) ?? null)
  const [picked, setPicked] = useState<string[]>([])
  const can = (a: HumanAction) => atLeast(roleIn(me, a.space_id), 'operator')
  const selectable = actions.filter((a) => isPending(a) && can(a))
  const chosen = selectable.filter((a) => picked.includes(a.id))
  const ackable = chosen.filter((a) => a.type === 'message')
  const current = open ? (actions.find((a) => a.id === open.id) ?? open) : null

  const bulk = async (label: string, list: HumanAction[], call: (a: HumanAction) => Promise<unknown>) => {
    let ok = 0
    for (const a of list) {
      try {
        await call(a)
        ok++
      } catch (e) {
        notifyError(e, `${a.title}: ${label} failed`)
      }
    }
    if (ok) notifyOk(`${label}: ${ok} of ${list.length}`)
    setPicked([])
    void qc.invalidateQueries({ queryKey: ['human'] })
  }
  const claim = (a: HumanAction) => api.POST('/spaces/{space_id}/human-actions/{action_id}/assign', {
    params: { path: { space_id: a.space_id, action_id: a.id }, header: { 'If-Match': ifMatch(a.version) } }, body: { assignee: me!.user!.id },
  }).then(unwrap)
  const ack = (a: HumanAction) => api.POST('/spaces/{space_id}/human-actions/{action_id}/respond', {
    params: { path: { space_id: a.space_id, action_id: a.id }, header: { 'If-Match': ifMatch(a.version) } }, body: { outcome: 'acknowledged' },
  }).then(unwrap)

  return (
    <>
      {selectable.length > 0 && (
        <Group gap="xs" mb={6} justify="space-between">
          <Checkbox size="xs" label={chosen.length ? `${chosen.length} selected` : 'Select all'} checked={chosen.length > 0 && chosen.length === selectable.length}
            indeterminate={chosen.length > 0 && chosen.length < selectable.length}
            onChange={(e) => setPicked(e.currentTarget.checked ? selectable.map((a) => a.id) : [])} />
          <Group gap="xs">
            {me?.user && <Button size="xs" variant="default" leftSection={<IconHandGrab size={14} />} disabled={chosen.length === 0}
              onClick={() => void bulk('Claim', chosen, claim)}>Claim</Button>}
            <Button size="xs" variant="default" leftSection={<IconCheck size={14} />} disabled={ackable.length === 0}
              onClick={() => void bulk('Acknowledge', ackable, ack)}>Acknowledge messages{ackable.length ? ` (${ackable.length})` : ''}</Button>
          </Group>
        </Group>
      )}
      <Stack gap={4} role="list" aria-label="Human actions">
        {actions.map((a) => (
          <Card key={a.id} p="xs" role="listitem" tabIndex={0} aria-label={`${a.type}: ${a.title}, ${a.status}`} style={{ cursor: 'pointer' }}
            onClick={() => setOpen(a.id)} onKeyDown={(e) => { if (e.key === 'Enter') setOpen(a.id) }}>
            <Group gap="xs" wrap="nowrap">
              {isPending(a) && can(a) && (
                <Checkbox size="xs" aria-label={`Select ${a.title}`} checked={picked.includes(a.id)} onClick={(e) => e.stopPropagation()}
                  onChange={(e) => setPicked(e.currentTarget.checked ? [...picked, a.id] : picked.filter((x) => x !== a.id))} />
              )}
              <TypeBadge type={a.type} slug={a.type_slug} />
              <Text size="sm" style={{ flex: 1 }} truncate>{a.title}</Text>
              <PriorityBadge priority={a.priority} />
              {a.type === 'message' && !(a.request as MessageReq)?.requires_ack && isPending(a) && <Badge variant="outline" color="gray">FYI</Badge>}
              {a.blocks_task && isPending(a) && <Badge variant="outline" color="owlYellow">blocking</Badge>}
              {spaceNames && <Badge variant="default">{spaceNames[a.space_id] ?? a.space_id}</Badge>}
              <Text size="xs" c="dimmed" visibleFrom="sm" w={150} truncate>{requesterLabel(a)}</Text>
              <Text size="xs" c="dimmed" w={70} ta="right">{ago(a.created_at)}</Text>
              <HumanStatusBadge action={a} />
            </Group>
          </Card>
        ))}
        {actions.length === 0 && <Text size="sm" c="dimmed" ta="center" p="lg">{empty}</Text>}
      </Stack>
      <Drawer opened={current !== null} onClose={() => setOpen(null)} position="right" size="lg" title="Human action">
        {current && <ActionDetail action={current} spaceName={spaceNames?.[current.space_id]} onChanged={setOpenState} />}
      </Drawer>
    </>
  )
}
