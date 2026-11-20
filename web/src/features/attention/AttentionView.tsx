import { Alert, Badge, Card, Group, List, Progress, Select, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { IconCircleCheck } from '@tabler/icons-react'
import { useState, type ReactNode } from 'react'
import type { Schemas } from '@/api/client'
import { isMiniApp } from '@/api/session'
import { AnchorLink } from '@/components/AnchorLink'
import { formatBytes } from '@/components/composer/upload'
import { ago, datetime } from '@/lib/format'
import { groupAttention, kindOptions, KIND_LABELS, spaceOptions, targetRoute, truncate, truncatedKinds, type Attention, type AttentionSpace, type Row } from './targets'

export type { Attention, AttentionSpace }

type SpaceRoute = '/spaces/$spaceId' | '/spaces/$spaceId/human' | '/spaces/$spaceId/queue' | '/spaces/$spaceId/events' | '/spaces/$spaceId/limits' | '/spaces/$spaceId/runs' | '/spaces/$spaceId/usage' | '/spaces/$spaceId/accounts' | '/spaces/$spaceId/cross-space'

/** The pages the Mini App does not have (their operations are excluded there): in the Mini App their counts stay, unlinked. */
const WEB_ONLY: SpaceRoute[] = ['/spaces/$spaceId/events', '/spaces/$spaceId/limits', '/spaces/$spaceId/usage', '/spaces/$spaceId/accounts', '/spaces/$spaceId/cross-space']

/** A count's link to the page where it is handled; in the Mini App a web-only page gets no link. */
function Linked({ to, spaceId, miniApp, children }: { to: SpaceRoute; spaceId: string; miniApp: boolean; children: ReactNode }) {
  if (miniApp && WEB_ONLY.includes(to)) return <>{children}</>
  return <AnchorLink to={to} params={{ spaceId }} size="sm" underline="never">{children}</AnchorLink>
}

/** One count that links to the page where it is handled; nothing when it is zero or unreadable. */
function Count({ n, label, to, spaceId, miniApp, color = 'owlYellow' }: { n?: number; label: string; to: SpaceRoute; spaceId: string; miniApp: boolean; color?: string }) {
  if (!n) return null
  const linked = !(miniApp && WEB_ONLY.includes(to))
  return (
    <Linked to={to} spaceId={spaceId} miniApp={miniApp}>
      <Badge variant="light" color={color} style={{ cursor: linked ? 'pointer' : undefined, textTransform: 'none' }}>{n} {label}</Badge>
    </Linked>
  )
}

/** The counts of one space as linked badges (SM-UI-003). In the Mini App the web-only ones are plain badges. */
export function SpaceCounts({ s, miniApp = false }: { s: AttentionSpace; miniApp?: boolean }) {
  const id = s.space_id
  const c = { spaceId: id, miniApp }
  return (
    <Group gap={4}>
      {s.node_disconnected && <AnchorLink to="/spaces/$spaceId" params={{ spaceId: id }} size="sm" underline="never"><Badge variant="light" color="owlRed" style={{ textTransform: 'none' }}>node offline</Badge></AnchorLink>}
      <Count n={s.human_pending} label="human actions" to="/spaces/$spaceId/human" {...c} />
      <Count n={s.human_overdue} label="overdue" to="/spaces/$spaceId/human" {...c} color="owlRed" />
      <Count n={s.human_escalated} label="escalated" to="/spaces/$spaceId/human" {...c} color="owlPurple" />
      <Count n={s.tasks_blocked} label="blocked tasks" to="/spaces/$spaceId/queue" {...c} />
      <Count n={s.tasks_failed} label="failed tasks" to="/spaces/$spaceId/queue" {...c} color="owlRed" />
      <Count n={s.quarantine_open} label="quarantined events" to="/spaces/$spaceId/events" {...c} color="owlPurple" />
      <Count n={s.runs_unknown} label="runs unknown" to="/spaces/$spaceId" {...c} color="owlPurple" />
      <Count n={s.runs_failed} label="runs failed (24 h)" to="/spaces/$spaceId" {...c} color="owlRed" />
      <Count n={s.budget_exceeded} label="budgets exceeded" to="/spaces/$spaceId/limits" {...c} color="owlRed" />
      <Count n={s.budget_warnings} label="budget warnings" to="/spaces/$spaceId/limits" {...c} />
      <Count n={s.runs_parked} label="runs parked" to="/spaces/$spaceId/runs" {...c} color="owlBlue" />
      <Count n={s.account_limits} label="account limits" to="/spaces/$spaceId/usage" {...c} color="owlBlue" />
      <Count n={s.account_switches_failed} label="account switches failed (24 h)" to="/spaces/$spaceId/accounts" {...c} color="owlRed" />
      <Count n={s.xspace_incoming} label="cross-space requests" to="/spaces/$spaceId/cross-space" {...c} />
      {s.storage_near_quota && (
        <Linked to="/spaces/$spaceId/limits" spaceId={id} miniApp={miniApp}>
          <Badge variant="light" color="owlPurple" style={{ textTransform: 'none' }}>attachments {formatBytes(s.storage_bytes ?? 0)} of {formatBytes(s.storage_quota_bytes ?? 0)}</Badge>
        </Linked>
      )}
    </Group>
  )
}

function Section({ title, ok, warn, children }: { title: string; ok: boolean; warn?: string; children: ReactNode }) {
  return (
    <Card>
      <Group justify="space-between" mb={6}>
        <Text fw={600}>{title}</Text>
        {!ok ? <Badge color="owlRed" variant="light">Needs attention</Badge>
          : warn ? <Badge color="owlYellow" variant="light">{warn}</Badge>
            : <Badge color="owlGreen" variant="light" leftSection={<IconCircleCheck size={12} />}>OK</Badge>}
      </Group>
      {children}
    </Card>
  )
}

/** Central's storage: readiness checks, the database's health and the data directory's free space. */
function StorageSection({ st }: { st: Schemas['AttentionStorage'] }) {
  return (
    <Section title="Storage" ok={!st.problem}>
      <Stack gap={6}>
        <Group gap="xs"><Text size="sm">Readiness</Text><Badge variant="outline" color={st.readiness === 'ready' ? 'owlGreen' : 'owlRed'}>{st.readiness}</Badge>
          <Text size="sm">Database</Text><Badge variant="outline" color={st.store_state === 'ok' ? 'owlGreen' : 'owlRed'}>{st.store_state}</Badge></Group>
        {st.store_reason && <Text size="sm" c="owlRed">{st.store_reason}</Text>}
        {st.disk_total_bytes > 0 && (
          <>
            <Text size="sm">Data disk: {formatBytes(st.disk_available_bytes)} free of {formatBytes(st.disk_total_bytes)} (reserve {formatBytes(st.disk_reserve_bytes)})</Text>
            <Progress value={st.disk_used_percent} color={st.below_reserve ? 'owlRed' : st.disk_used_percent > 85 ? 'owlPurple' : 'owlBlue'}
              aria-label={`Data disk ${Math.round(st.disk_used_percent)}% used`} />
          </>
        )}
        {st.disk_error && <Text size="sm" c="owlRed">The data directory's free space cannot be read.</Text>}
        {st.checks.filter((c) => c.status !== 'ok').length > 0 && (
          <List size="sm">{st.checks.filter((c) => c.status !== 'ok').map((c) => <List.Item key={c.name}><span className="mono">{c.name}</span>: {c.status}{c.error ? ` (${c.error})` : ''}</List.Item>)}</List>
        )}
        <Text size="xs" c="dimmed">Node disks are not reported here; a recording cut short by a full node disk shows on its run.</Text>
      </Stack>
    </Section>
  )
}

/** True when the space has anything to show: its total, or rows only in the watch categories. */
export function spaceHasItems(s: AttentionSpace): boolean {
  return s.total > 0 || [s.runs_parked, s.account_limits, s.account_switches_failed, s.xspace_incoming].some((n) => !!n)
}

function ItemRow({ row, names, miniApp }: { row: Row; names: Map<string, string>; miniApp: boolean }) {
  if (row.type === 'human') {
    const where = row.spaceId ? names.get(row.spaceId) : undefined
    const label = `${row.count} human action${row.count === 1 ? '' : 's'}${row.overdue ? ` (${row.overdue} overdue)` : ''}${row.escalated ? ` (${row.escalated} escalated)` : ''}`
    return (
      <Group gap="xs" wrap="nowrap" justify="space-between" py={4}>
        <Group gap="xs">
          <Badge variant="light" color="owlYellow" style={{ textTransform: 'none' }}>Human actions</Badge>
          <Text size="sm">{label}{where ? ` in ${where}` : ''}</Text>
        </Group>
        {row.spaceId
          ? <AnchorLink to="/spaces/$spaceId/human" params={{ spaceId: row.spaceId }} size="sm">Open Needs you</AnchorLink>
          : <AnchorLink to="/human" size="sm">Open Needs you</AnchorLink>}
      </Group>
    )
  }
  const { item } = row
  const route = targetRoute(item.target, miniApp)
  const title = truncate(item.title)
  return (
    <Group gap="xs" wrap="nowrap" justify="space-between" py={4}>
      <Group gap="xs" style={{ minWidth: 0 }}>
        <Badge variant="light" color={item.severity === 'act' ? 'owlRed' : 'owlBlue'} style={{ textTransform: 'none', flexShrink: 0 }}>{KIND_LABELS[item.kind] ?? item.kind}</Badge>
        {route ? <AnchorLink {...(route as unknown as { to: '/' })} size="sm">{title}</AnchorLink> : <Text size="sm">{title}</Text>}
      </Group>
      <Text size="xs" c="dimmed" style={{ flexShrink: 0 }}>{names.get(item.space_id) ?? item.space_id} · {ago(item.since)}</Text>
    </Group>
  )
}

function RowGroup({ title, rows, names, miniApp, empty }: { title: string; rows: Row[]; names: Map<string, string>; miniApp: boolean; empty: string }) {
  return (
    <Stack gap={4} component="section" aria-label={title}>
      <Title order={4}>{title} <Text span c="dimmed" fw={400}>{rows.length}</Text></Title>
      {rows.length === 0 ? <Text size="sm" c="dimmed">{empty}</Text> : rows.map((r) => <ItemRow key={r.key} row={r} names={names} miniApp={miniApp} />)}
    </Stack>
  )
}

/**
 * Everything that needs a person (SM-UI-003): the itemised rows grouped Act now and Watch, filtered by space and kind,
 * with human actions collapsed into one row that opens Needs you; then the installation sections. The rows come from
 * `items[]`. In the Mini App the sections keep their text but drop the links to pages the Mini App does not have.
 */
export function AttentionView({ data }: { data: Attention }) {
  const [space, setSpace] = useState<string | null>(null)
  const [kind, setKind] = useState<string | null>(null)
  const source = { items: data.items ?? [], spaces: data.spaces ?? [] }
  const grouped = groupAttention(source, { space: space ?? undefined, kind: kind ?? undefined })
  const names = new Map(spaceOptions(source).map((o) => [o.value, o.label]))
  const cut = truncatedKinds({ items: source.items, item_counts: data.item_counts ?? {} })
  const empty = grouped.act.length + grouped.watch.length === 0
  const nodes = data.nodes ?? []
  const miniApp = isMiniApp()
  return (
    <Stack gap="md">
      {data.total === 0 && source.items.length === 0 ? (
        <Alert color="owlGreen" icon={<IconCircleCheck size={16} />} title="Nothing needs you">Everything you can see is in order.</Alert>
      ) : (
        <Text size="sm" c="dimmed">{data.total} item{data.total === 1 ? '' : 's'} need a person · checked {ago(data.checked_at)}</Text>
      )}
      {(source.items.length > 0 || source.spaces.some((s) => (s.human_pending ?? 0) > 0)) && (
        <>
          <Group gap="sm">
            <Select aria-label="Filter by space" placeholder="All spaces" clearable data={spaceOptions(source)} value={space} onChange={setSpace} />
            <Select aria-label="Filter by kind" placeholder="All kinds" clearable data={kindOptions(source)} value={kind} onChange={setKind} />
          </Group>
          <RowGroup title="Act now" rows={grouped.act} names={names} miniApp={miniApp} empty="Nothing needs action." />
          <RowGroup title="Watch" rows={grouped.watch} names={names} miniApp={miniApp} empty="Nothing to watch." />
          {empty && (space || kind) && <Text size="sm" c="dimmed">No item matches these filters.</Text>}
          {cut.length > 0 && <Text size="xs" c="dimmed">Showing the newest of each kind: {cut.map((c) => `${c.shown} of ${c.total} ${(KIND_LABELS[c.kind as keyof typeof KIND_LABELS] ?? c.kind).toLowerCase()}`).join(', ')}.</Text>}
        </>
      )}
      {(data.nodes !== undefined || data.outbox || data.backups || data.storage) && (
        <SimpleGrid cols={{ base: 1, md: 2 }}>
          {data.nodes !== undefined && (
            <Section title="Nodes" ok={nodes.length === 0}>
              {nodes.length === 0 ? <Text size="sm" c="dimmed">Every enrolled node is connected.</Text> : (
                <List size="sm">
                  {nodes.map((n) => (
                    <List.Item key={n.id}>
                      {miniApp ? n.name : <AnchorLink to="/nodes" size="sm">{n.name}</AnchorLink>} is not connected{n.last_disconnect_at ? ` since ${datetime(n.last_disconnect_at)}` : ''};
                      {' '}{n.spaces.length} space{n.spaces.length === 1 ? '' : 's'} start no new work.
                    </List.Item>
                  ))}
                </List>
              )}
            </Section>
          )}
          {data.outbox && (
            <Section title="Outbound events" ok={data.outbox.dead === 0}>
              <Text size="sm">{data.outbox.dead} dead letter{data.outbox.dead === 1 ? '' : 's'}, {data.outbox.retrying} retrying.</Text>
              {data.outbox.dead > 0 && !miniApp && <AnchorLink to="/events" size="sm">See the outbound status on Events</AnchorLink>}
            </Section>
          )}
          {data.backups && (
            <Section title="Backups" ok={data.backups.state !== 'failing'} warn={data.backups.state === 'none' ? 'Never backed up' : undefined}>
              <Text size="sm">
                {data.backups.state === 'none' ? 'No backup has run yet: nothing could be restored.' : `Last good backup ${data.backups.last_succeeded_at ? datetime(data.backups.last_succeeded_at) : 'never'}.`}
                {data.backups.last_failed_at && ` Last failure ${datetime(data.backups.last_failed_at)}.`}
                {data.backups.last_verify_failed_at && ` Last failed verification ${datetime(data.backups.last_verify_failed_at)}.`}
              </Text>
              {!miniApp && <AnchorLink to="/backups" size="sm">Backups</AnchorLink>}
            </Section>
          )}
          {data.storage && <StorageSection st={data.storage} />}
        </SimpleGrid>
      )}
    </Stack>
  )
}
