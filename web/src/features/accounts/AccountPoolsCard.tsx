import { ActionIcon, Badge, Button, Card, Group, Select, Stack, Table, Text, Title, VisuallyHidden } from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconX } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { api, ifMatch, unwrap, type AccountBinding } from '@/api/client'
import { keys, useMe, useRolePools, useSpaceAccounts, useSpaceRoles } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { atLeast, roleIn } from '@/lib/access'
import { notifyError, notifyOk } from '@/lib/notify'
import { inPoolOrder, moveEntry, withPriorities } from './pools'

const harnessLabel = (h?: string) => (h === 'codex' ? 'Codex' : 'Claude Code')

/** Up and down buttons for one row of an ordered list; the order is the failover order. */
function Mover({ label, index, count, onMove, disabled }: { label: string; index: number; count: number; onMove: (delta: number) => void; disabled?: boolean }) {
  return (
    <Group gap={4} wrap="nowrap">
      <ActionIcon variant="subtle" aria-label={`Move ${label} earlier`} disabled={disabled || index === 0} onClick={() => onMove(-1)}><IconArrowUp size={14} /></ActionIcon>
      <ActionIcon variant="subtle" aria-label={`Move ${label} later`} disabled={disabled || index === count - 1} onClick={() => onMove(1)}><IconArrowDown size={14} /></ActionIcon>
    </Group>
  )
}

/**
 * The failover order of a space's accounts, and the role pools (ADR-0090). With usage-limit failover on, a run that
 * reaches its account's limit moves to the next eligible account of its pool: the role's pool when it has one,
 * otherwise the space's bound accounts of its harness, lowest priority first. Names only, never credentials.
 */
export function AccountPoolsCard({ spaceId }: { spaceId: string }) {
  const bound = useSpaceAccounts(spaceId)
  const pools = useRolePools(spaceId)
  const roles = useSpaceRoles(spaceId)
  const canWrite = atLeast(roleIn(useMe().data, spaceId), 'admin')
  const version = pools.data?.accounts_version
  return (
    <Card data-testid="account-pools">
      <Title order={5} mb={4}>Pool order</Title>
      <Text size="sm" c="dimmed" mb="sm">
        When failover is on, a run at its account's usage limit moves to the next eligible account, first in this order. A role with its own
        pool below uses only that pool.
      </Text>
      <ErrorAlert error={bound.error ?? pools.error} />
      {(bound.data ?? []).length === 0 ? <Text size="sm" c="dimmed">No account is bound to this space.</Text> : (
        (['claude-code', 'codex'] as const).map((h) => <BindingOrder key={h} spaceId={spaceId} harness={h} all={bound.data ?? []} version={version} canWrite={canWrite} />)
      )}
      <Title order={5} mt="lg" mb={4}>Role pools</Title>
      <Text size="sm" c="dimmed" mb="sm">A role with a pool uses those accounts only, each must also be bound to this space. A role without one uses the order above.</Text>
      <Stack gap="md">
        {(roles.data ?? []).map((r) => (
          <RolePoolEditor key={r.name} spaceId={spaceId} role={r.name} entries={pools.data?.items.find((p) => p.role === r.name)?.items ?? []}
            bound={bound.data ?? []} version={version} canWrite={canWrite} />
        ))}
        {roles.data?.length === 0 && <Text size="sm" c="dimmed">This space has no roles.</Text>}
      </Stack>
    </Card>
  )
}

function BindingOrder({ spaceId, harness, all, version, canWrite }: { spaceId: string; harness: 'claude-code' | 'codex'; all: AccountBinding[]; version?: number; canWrite: boolean }) {
  const qc = useQueryClient()
  const mine = all.filter((b) => b.harness === harness)
  const saved = inPoolOrder(mine).map((b) => b.account_id).join(',')
  const [order, setOrder] = useState(() => saved.split(',').filter(Boolean))
  const [busy, setBusy] = useState(false)
  // A change from elsewhere (the change feed) replaces the local order.
  useEffect(() => setOrder(saved.split(',').filter(Boolean)), [saved])
  if (mine.length === 0) return null
  const dirty = order.join(',') !== saved
  const byId = new Map(mine.map((b) => [b.account_id, b]))
  async function save() {
    setBusy(true)
    try {
      // The PUT replaces the whole binding list, so the other harness's bindings go back unchanged.
      const rank = new Map(withPriorities(order.map((id) => ({ account_id: id }))).map((e) => [e.account_id, e.priority]))
      const items = all.map((b) => ({ account_id: b.account_id, default: b.default, priority: rank.get(b.account_id) ?? b.priority }))
      unwrap(await api.PUT('/spaces/{space_id}/accounts', { params: { path: { space_id: spaceId }, header: version === undefined ? {} : { 'If-Match': ifMatch(version) } }, body: { items } }))
      void qc.invalidateQueries({ queryKey: keys.spaceAccounts(spaceId) })
      void qc.invalidateQueries({ queryKey: keys.rolePools(spaceId) })
      notifyOk('Pool order saved')
    } catch (e) {
      notifyError(e, 'Could not save the pool order')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Stack gap={4} mb="sm">
      <Text fw={600} size="sm">{harnessLabel(harness)}</Text>
      <Table aria-label={`${harnessLabel(harness)} pool order`}>
        <Table.Thead><Table.Tr><Table.Th w={50}>#</Table.Th><Table.Th>Account</Table.Th>{canWrite && <Table.Th w={90}><VisuallyHidden>Order</VisuallyHidden></Table.Th>}</Table.Tr></Table.Thead>
        <Table.Tbody>
          {order.map((id, i) => {
            const b = byId.get(id)
            if (!b) return null
            return (
              <Table.Tr key={id}>
                <Table.Td>{i + 1}</Table.Td>
                <Table.Td>
                  <Group gap="xs"><Text size="sm">{b.name}</Text>{b.default && <Badge size="xs" variant="light">default</Badge>}{!b.enabled && <Badge size="xs" color="gray">disabled</Badge>}</Group>
                </Table.Td>
                {canWrite && <Table.Td><Mover label={b.name ?? id} index={i} count={order.length} onMove={(d) => setOrder(moveEntry(order, i, d))} /></Table.Td>}
              </Table.Tr>
            )
          })}
        </Table.Tbody>
      </Table>
      {canWrite && dirty && (
        <Group justify="flex-end"><Button size="xs" variant="default" onClick={() => setOrder(saved.split(','))}>Reset</Button><Button size="xs" loading={busy} onClick={() => void save()}>Save order</Button></Group>
      )}
    </Stack>
  )
}

function RolePoolEditor({ spaceId, role, entries, bound, version, canWrite }: { spaceId: string; role: string; entries: { account_id: string; name?: string; priority?: number }[]; bound: AccountBinding[]; version?: number; canWrite: boolean }) {
  const qc = useQueryClient()
  const saved = inPoolOrder(entries).map((e) => e.account_id).join(',')
  const [order, setOrder] = useState(() => saved.split(',').filter(Boolean))
  const [adding, setAdding] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => setOrder(saved.split(',').filter(Boolean)), [saved])
  const dirty = order.join(',') !== saved
  const name = (id: string) => bound.find((b) => b.account_id === id)?.name ?? entries.find((e) => e.account_id === id)?.name ?? id
  const candidates = bound.filter((b) => !order.includes(b.account_id)).map((b) => ({ value: b.account_id, label: `${b.name} · ${harnessLabel(b.harness)}` }))
  async function save() {
    setBusy(true)
    try {
      const items = withPriorities(order.map((id) => ({ account_id: id })))
      unwrap(await api.PUT('/spaces/{space_id}/account-pools/{role}', {
        params: { path: { space_id: spaceId, role }, header: version === undefined ? {} : { 'If-Match': ifMatch(version) } }, body: { items },
      }))
      void qc.invalidateQueries({ queryKey: keys.rolePools(spaceId) })
      notifyOk(order.length ? `Pool of ${role} saved` : `${role} uses the space's accounts again`)
    } catch (e) {
      notifyError(e, 'Could not save the pool')
    } finally {
      setBusy(false)
    }
  }
  return (
    <Stack gap={4} data-testid={`role-pool-${role}`}>
      <Group gap="xs"><Text fw={600} size="sm">{role}</Text>{order.length === 0 && <Badge size="xs" variant="light" color="gray">uses the space's accounts</Badge>}</Group>
      {order.length > 0 && (
        <Table aria-label={`Pool of ${role}`}>
          <Table.Tbody>
            {order.map((id, i) => (
              <Table.Tr key={id}>
                <Table.Td w={50}>{i + 1}</Table.Td>
                <Table.Td><Text size="sm">{name(id)}</Text></Table.Td>
                {canWrite && (
                  <Table.Td w={130}>
                    <Group gap={4} wrap="nowrap">
                      <Mover label={name(id)} index={i} count={order.length} onMove={(d) => setOrder(moveEntry(order, i, d))} />
                      <ActionIcon variant="subtle" color="owlRed" aria-label={`Remove ${name(id)} from the pool of ${role}`} onClick={() => setOrder(order.filter((x) => x !== id))}><IconX size={14} /></ActionIcon>
                    </Group>
                  </Table.Td>
                )}
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {canWrite && (
        <Group gap="xs" align="flex-end" wrap="wrap">
          <Select size="xs" aria-label={`Add an account to the pool of ${role}`} placeholder="Add an account" data={candidates} value={adding} searchable={false}
            disabled={candidates.length === 0} onChange={(v) => { if (v) { setOrder([...order, v]); setAdding(null) } }} />
          {dirty && <Button size="xs" variant="default" onClick={() => setOrder(saved.split(',').filter(Boolean))}>Reset</Button>}
          {dirty && <Button size="xs" loading={busy} onClick={() => void save()}>{order.length ? 'Save pool' : 'Remove pool'}</Button>}
        </Group>
      )}
    </Stack>
  )
}
