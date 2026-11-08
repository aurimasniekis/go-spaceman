import { Badge, Card, Code, Group, SimpleGrid, Stack, Text } from '@mantine/core'
import type { ReactNode } from 'react'
import type { Schemas } from '@/api/client'
import { bytes, datetime } from '@/lib/format'

export type SystemStatus = Schemas['SystemStatus']

/** Formats an uptime in seconds as its two largest units: "45s", "12m 5s", "3h 20m", "2d 4h". */
export function uptime(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  if (m > 0) return `${m}m ${s % 60}s`
  return `${s}s`
}

/** Whether the single writer took its lock in the last readiness evaluation. */
export function acceptingWrites(st: SystemStatus['storage']): boolean | undefined {
  const c = st.checks.find((x) => x.name === 'store.writable')
  return c ? c.status === 'ok' : undefined
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" fw={600}>{label}</Text>
      <div>{children}</div>
    </Stack>
  )
}

function CardTitle({ children }: { children: ReactNode }) {
  return <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb="xs">{children}</Text>
}

const Unavailable = () => <Badge color="owlRed" variant="light">unavailable</Badge>

/** A count, or "unavailable" when its source failed (the status degrades that section instead of failing). */
function Count({ value, error }: { value: ReactNode; error?: string }) {
  return error ? <Unavailable /> : <Text size="lg" fw={600}>{value}</Text>
}

function Ok({ ok, yes, no }: { ok: boolean | undefined; yes: string; no: string }) {
  if (ok === undefined) return <Badge color="gray" variant="light">unknown</Badge>
  return <Badge color={ok ? 'owlGreen' : 'owlRed'} variant="light">{ok ? yes : no}</Badge>
}

/** This instance's status for administrators (SM-OBS-006): health, build and versions, activity and where it runs. */
export function SystemView({ s }: { s: SystemStatus }) {
  const st = s.storage
  const writes = acceptingWrites(st)
  const commit = s.build.commit ? s.build.commit.slice(0, 12) : 'unknown'
  const schemaOk = !s.schema.error && s.schema.current === s.schema.latest
  return (
    <Stack gap="md">
      <Card data-testid="system-status">
        <CardTitle>Status</CardTitle>
        <SimpleGrid cols={{ base: 1, xs: 2, md: 3 }} spacing="md">
          <Field label="Readiness"><Badge color={st.readiness === 'ready' ? 'owlGreen' : st.readiness === 'degraded' ? 'owlYellow' : 'owlRed'} variant="light">{st.readiness}</Badge></Field>
          <Field label="Storage"><Ok ok={writes} yes="Accepting writes" no="Not accepting writes" /></Field>
          <Field label="Database"><Badge color={st.store_state === 'ok' ? 'owlGreen' : 'owlRed'} variant="light">{st.store_state}</Badge>
            {st.store_reason && <Text size="xs" c="owlRed">{st.store_reason}</Text>}</Field>
          <Field label="Version"><Text size="sm" className="mono">{s.build.version}</Text></Field>
          <Field label="Commit"><Text size="sm" className="mono">{commit}</Text>{s.build.date && <Text size="xs" c="dimmed">built {datetime(s.build.date)}</Text>}</Field>
          <Field label="Boot ID"><Text size="sm" className="mono" style={{ wordBreak: 'break-all' }}>{s.boot_id || '—'}</Text></Field>
          <Field label="Started"><Text size="sm">{datetime(s.started_at)}</Text><Text size="xs" c="dimmed">up {uptime(s.uptime_seconds)}</Text></Field>
          <Field label="Database schema">
            {s.schema.error ? <Unavailable /> : (
              <Group gap={6}><Text size="sm" className="mono">{s.schema.current}</Text>
                {!schemaOk && <Badge color="owlYellow" variant="light">binary has {s.schema.latest}</Badge>}</Group>
            )}
          </Field>
          <Field label="Node protocol"><Text size="sm" className="mono">{s.protocol.major}.{s.protocol.min_minor} – {s.protocol.major}.{s.protocol.max_minor}</Text></Field>
        </SimpleGrid>
      </Card>
      <Card data-testid="system-counts">
        <CardTitle>Activity</CardTitle>
        <SimpleGrid cols={{ base: 2, sm: 3, md: 5 }} spacing="md">
          <Field label="Spaces"><Count value={s.counts.spaces} error={s.counts.error} /></Field>
          <Field label="Nodes connected"><Count value={<>{s.counts.nodes_connected} <Text span size="sm" c="dimmed">of {s.counts.nodes_active}</Text></>} error={s.counts.nodes_error} /></Field>
          <Field label="Runs running"><Count value={s.counts.runs_running} error={s.counts.error} /></Field>
          <Field label="Pending human actions"><Count value={s.counts.human_pending} error={s.counts.error} /></Field>
          <Field label="Live streams"><Count value={s.counts.live_streams} /></Field>
        </SimpleGrid>
      </Card>
      <Card data-testid="system-instance">
        <CardTitle>This instance</CardTitle>
        <SimpleGrid cols={{ base: 1, sm: 2 }} spacing="md">
          <Field label="Public URL"><Code style={{ wordBreak: 'break-all' }}>{s.public_url || '—'}</Code></Field>
          <Field label="Data directory"><Code style={{ wordBreak: 'break-all' }}>{s.data_dir || '—'}</Code></Field>
          <Field label="Database size"><Text size="sm">{bytes(st.db_bytes)}<Text span size="xs" c="dimmed"> + {bytes(st.wal_bytes)} WAL</Text></Text></Field>
          <Field label="Data disk">
            {st.disk_error ? <Text size="sm" c="owlRed">free space unavailable</Text>
              : <Text size="sm">{bytes(st.disk_available_bytes)} free of {bytes(st.disk_total_bytes)}{st.below_reserve && <Badge ml={6} color="owlRed" variant="light">below reserve</Badge>}</Text>}
          </Field>
          <Field label="Go"><Text size="sm" className="mono">{s.build.go_version}</Text></Field>
          <Field label="Platform"><Text size="sm" className="mono">{s.build.platform}</Text></Field>
        </SimpleGrid>
      </Card>
    </Stack>
  )
}
