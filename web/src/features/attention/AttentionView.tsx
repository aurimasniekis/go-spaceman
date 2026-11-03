import { Alert, Badge, Card, Group, List, Progress, SimpleGrid, Stack, Text, Title } from '@mantine/core'
import { IconCircleCheck } from '@tabler/icons-react'
import type { ReactNode } from 'react'
import type { Schemas } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { formatBytes } from '@/components/composer/upload'
import { DataTable, type ColumnDef } from '@/components/DataTable'
import { ago, datetime } from '@/lib/format'

export type Attention = Schemas['Attention']
export type AttentionSpace = Schemas['AttentionSpace']

type SpaceRoute = '/spaces/$spaceId' | '/spaces/$spaceId/human' | '/spaces/$spaceId/queue' | '/spaces/$spaceId/events' | '/spaces/$spaceId/limits'

/** One count that links to the page where it is handled; nothing when it is zero or unreadable. */
function Count({ n, label, to, spaceId, color = 'owlYellow' }: { n?: number; label: string; to: SpaceRoute; spaceId: string; color?: string }) {
  if (!n) return null
  return (
    <AnchorLink to={to} params={{ spaceId }} size="sm" underline="never">
      <Badge variant="light" color={color} style={{ cursor: 'pointer', textTransform: 'none' }}>{n} {label}</Badge>
    </AnchorLink>
  )
}

/** The counts of one space as linked badges (SM-UI-003). */
export function SpaceCounts({ s }: { s: AttentionSpace }) {
  const id = s.space_id
  return (
    <Group gap={4}>
      {s.node_disconnected && <AnchorLink to="/spaces/$spaceId" params={{ spaceId: id }} size="sm" underline="never"><Badge variant="light" color="owlRed" style={{ textTransform: 'none' }}>node offline</Badge></AnchorLink>}
      <Count n={s.human_pending} label="human actions" to="/spaces/$spaceId/human" spaceId={id} />
      <Count n={s.human_overdue} label="overdue" to="/spaces/$spaceId/human" spaceId={id} color="owlRed" />
      <Count n={s.human_escalated} label="escalated" to="/spaces/$spaceId/human" spaceId={id} color="owlPurple" />
      <Count n={s.tasks_blocked} label="blocked tasks" to="/spaces/$spaceId/queue" spaceId={id} />
      <Count n={s.tasks_failed} label="failed tasks" to="/spaces/$spaceId/queue" spaceId={id} color="owlRed" />
      <Count n={s.quarantine_open} label="quarantined events" to="/spaces/$spaceId/events" spaceId={id} color="owlPurple" />
      <Count n={s.runs_unknown} label="runs unknown" to="/spaces/$spaceId" spaceId={id} color="owlPurple" />
      <Count n={s.runs_failed} label="runs failed (24 h)" to="/spaces/$spaceId" spaceId={id} color="owlRed" />
      <Count n={s.budget_exceeded} label="budgets exceeded" to="/spaces/$spaceId/limits" spaceId={id} color="owlRed" />
      <Count n={s.budget_warnings} label="budget warnings" to="/spaces/$spaceId/limits" spaceId={id} />
      {s.storage_near_quota && (
        <AnchorLink to="/spaces/$spaceId/limits" params={{ spaceId: id }} size="sm" underline="never">
          <Badge variant="light" color="owlPurple" style={{ textTransform: 'none' }}>attachments {formatBytes(s.storage_bytes ?? 0)} of {formatBytes(s.storage_quota_bytes ?? 0)}</Badge>
        </AnchorLink>
      )}
    </Group>
  )
}

const columns: ColumnDef<AttentionSpace, unknown>[] = [
  {
    id: 'space', header: 'Space', accessorFn: (s) => s.name,
    cell: ({ row }) => <AnchorLink to="/spaces/$spaceId" params={{ spaceId: row.original.space_id }} size="sm" fw={500}>{row.original.name}</AnchorLink>,
  },
  { id: 'total', header: 'Items', accessorFn: (s) => s.total, size: 80, cell: ({ getValue }) => <Text size="sm" fw={600}>{getValue() as number}</Text> },
  { id: 'human', header: 'Human actions', accessorFn: (s) => s.human_pending ?? 0, size: 120 },
  { id: 'tasks', header: 'Blocked or failed tasks', accessorFn: (s) => (s.tasks_blocked ?? 0) + (s.tasks_failed ?? 0), size: 120 },
  {
    id: 'what', header: 'What needs you', enableSorting: false, accessorFn: (s) => s.slug,
    cell: ({ row }) => <SpaceCounts s={row.original} />,
  },
]

function Section({ title, ok, children }: { title: string; ok: boolean; children: ReactNode }) {
  return (
    <Card>
      <Group justify="space-between" mb={6}>
        <Text fw={600}>{title}</Text>
        {ok ? <Badge color="owlGreen" variant="light" leftSection={<IconCircleCheck size={12} />}>ok</Badge> : <Badge color="owlRed" variant="light">needs attention</Badge>}
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

/** Everything that needs a person (SM-UI-003), per space and for the installation where the caller may read it. */
export function AttentionView({ data }: { data: Attention }) {
  const nodes = data.nodes ?? []
  return (
    <Stack gap="md">
      {data.total === 0 ? (
        <Alert color="owlGreen" icon={<IconCircleCheck size={16} />} title="Nothing needs you">Everything you can see is in order.</Alert>
      ) : (
        <Text size="sm" c="dimmed">{data.total} item{data.total === 1 ? '' : 's'} need a person · checked {ago(data.checked_at)}</Text>
      )}
      <Stack gap={6}>
        <Title order={4}>Spaces</Title>
        <DataTable view="attention.spaces" data={data.spaces} columns={columns} getRowId={(s) => s.space_id} filterLabel="Filter spaces"
          defaultSort={[{ id: 'total', desc: true }]} caption="Spaces that need attention" empty="No space needs attention." />
      </Stack>
      {(data.nodes !== undefined || data.outbox || data.backups || data.storage) && (
        <SimpleGrid cols={{ base: 1, md: 2 }}>
          {data.nodes !== undefined && (
            <Section title="Nodes" ok={nodes.length === 0}>
              {nodes.length === 0 ? <Text size="sm" c="dimmed">Every enrolled node is connected.</Text> : (
                <List size="sm">
                  {nodes.map((n) => (
                    <List.Item key={n.id}>
                      <AnchorLink to="/nodes" size="sm">{n.name}</AnchorLink> is not connected{n.last_disconnect_at ? ` since ${datetime(n.last_disconnect_at)}` : ''};
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
              {data.outbox.dead > 0 && <AnchorLink to="/events" size="sm">See the outbound status on Events</AnchorLink>}
            </Section>
          )}
          {data.backups && (
            <Section title="Backups" ok={data.backups.state !== 'failing'}>
              <Text size="sm">
                {data.backups.state === 'none' ? 'No backup has run yet.' : `Last good backup ${data.backups.last_succeeded_at ? datetime(data.backups.last_succeeded_at) : 'never'}.`}
                {data.backups.last_failed_at && ` Last failure ${datetime(data.backups.last_failed_at)}.`}
                {data.backups.last_verify_failed_at && ` Last failed verification ${datetime(data.backups.last_verify_failed_at)}.`}
              </Text>
              <AnchorLink to="/backups" size="sm">Backups</AnchorLink>
            </Section>
          )}
          {data.storage && <StorageSection st={data.storage} />}
        </SimpleGrid>
      )}
    </Stack>
  )
}
