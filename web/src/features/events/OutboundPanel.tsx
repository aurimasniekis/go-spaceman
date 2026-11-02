import { Alert, Badge, Card, Group, SimpleGrid, Stack, Table, Text } from '@mantine/core'
import { useOutbound } from '@/api/queries'
import type { OutboundStatus } from '@/api/client'
import { ErrorAlert } from '@/components/ErrorAlert'
import { ago, datetime } from '@/lib/format'

/** Human-readable publication lag ("—" when nothing waits). */
export function lagText(seconds: number): string {
  if (seconds <= 0) return '—'
  if (seconds < 60) return `${Math.round(seconds)} s`
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`
  return `${(seconds / 3600).toFixed(1)} h`
}

function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed">{label}</Text>
      <Text fw={600} className="mono">{value}</Text>
    </Stack>
  )
}

function state(s: OutboundStatus): { label: string; color: string } {
  if (!s.enabled) return { label: 'disabled', color: 'gray' }
  if (!s.connected) return { label: 'not connected', color: 'owlYellow' }
  return { label: 'publishing', color: 'owlGreen' }
}

/** Outbound publication of SpaceMan's own events to Pulse (SM-OBS-004): connection, backlog, lag and failures. */
export function OutboundPanel({ enabled }: { enabled: boolean }) {
  const q = useOutbound(enabled)
  const s = q.data
  return (
    <Stack>
      <Text size="sm" c="dimmed">
        SpaceMan publishes its own lifecycle events (runs, tasks, inbox items, human actions, approvals, memory and KB changes) to Pulse through
        a transactional outbox. Payloads carry identifiers and states only.
      </Text>
      <ErrorAlert error={q.error} />
      {s && (
        <Card>
          <Stack>
            <Group justify="space-between">
              <Group gap="xs">
                <Text fw={600}>Outbound publication</Text>
                <Badge color={state(s).color}>{state(s).label}</Badge>
              </Group>
              <Text size="xs" c="dimmed" className="mono">source {s.source}{s.url ? ` · ${s.url}` : ''}</Text>
            </Group>
            {!s.enabled && (
              <Alert color="owlBlue">
                Publication is off, so no events are recorded. Enable it with <code>events.outbound</code> in the central configuration.
              </Alert>
            )}
            <SimpleGrid cols={{ base: 2, sm: 4 }}>
              <Stat label="Pending" value={s.pending} />
              <Stat label="Retrying" value={s.retrying} />
              <Stat label="Dead-lettered" value={s.dead} />
              <Stat label="Lag (oldest pending)" value={lagText(s.lag_seconds)} />
              <Stat label="Published (retained)" value={s.published} />
              <Stat label="Published since start" value={s.published_since_start} />
              <Stat label="Failed attempts since start" value={s.failed_since_start} />
              <Stat label="Last published" value={s.last_published_at ? ago(s.last_published_at) : '—'} />
            </SimpleGrid>
            {s.last_error && (
              <Alert color="owlRed" title={`Last error ${s.last_error_at ? ago(s.last_error_at) : ''}`}>
                <Text size="sm" className="mono">{s.last_error}</Text>
              </Alert>
            )}
            <Text size="xs" c="dimmed">Published and dead-lettered events are kept for {s.retention_hours} h.</Text>
          </Stack>
        </Card>
      )}
      {s && s.recent_failures.length > 0 && (
        <Card p={0}>
          <Table>
            <Table.Thead>
              <Table.Tr><Table.Th>Event</Table.Th><Table.Th>Type</Table.Th><Table.Th>Attempts</Table.Th><Table.Th>State</Table.Th><Table.Th>Last error</Table.Th><Table.Th>Created</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {s.recent_failures.map((f) => (
                <Table.Tr key={f.id}>
                  <Table.Td className="mono">{f.id}</Table.Td>
                  <Table.Td className="mono">{f.type}</Table.Td>
                  <Table.Td>{f.attempts}</Table.Td>
                  <Table.Td><Badge color={f.dead_at ? 'owlRed' : 'owlYellow'}>{f.dead_at ? 'dead' : 'retrying'}</Badge></Table.Td>
                  <Table.Td><Text size="xs" className="mono">{f.last_error}</Text></Table.Td>
                  <Table.Td><Text size="xs" c="dimmed">{datetime(f.created_at)}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Card>
      )}
    </Stack>
  )
}
