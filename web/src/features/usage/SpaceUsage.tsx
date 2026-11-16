import { Alert, Card, Group, SegmentedControl, SimpleGrid, Stack, Table, Text, Title } from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
import { useEffect, useMemo, useState } from 'react'
import type { UsageRow } from '@/api/client'
import { useSpaceAccounts, useSpaceUsage } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { RANGES, rangeOf, type RangeKey } from './range'
import { bucketStarts, buildChart, compact, estimate, groupLabel } from './series'
import { UsageChart, UsageLegend } from './UsageChart'

type GroupBy = 'role' | 'account' | 'model' | 'harness'
const GROUPS: { value: GroupBy; label: string }[] = [
  { value: 'role', label: 'Role' }, { value: 'account', label: 'Account' }, { value: 'model', label: 'Model' }, { value: 'harness', label: 'Harness' },
]
const n = (v: number) => v.toLocaleString('en-US')

/** One row per group over the whole range, biggest first (the table under the chart). */
function perGroup(rows: UsageRow[]) {
  const m = new Map<string, UsageRow>()
  for (const r of rows) {
    const a = m.get(r.group)
    if (!a) { m.set(r.group, { ...r, bucket_start: undefined }); continue }
    for (const k of ['input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens', 'requests', 'total_tokens', 'total_cost_usd'] as const) a[k] += r[k]
  }
  return [...m.values()].sort((a, b) => b.total_tokens - a.total_tokens || a.group.localeCompare(b.group))
}

const fmtBucket = (bucket: 'hour' | 'day') => (iso: string) => {
  const d = new Date(iso)
  return bucket === 'hour' ? `${String(d.getUTCHours()).padStart(2, '0')}:00` : d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
}

/**
 * A space's token usage from the usage ledger (AS-T7). Tokens are the primary unit; the cost is the harness's own
 * list-price estimate and is labelled so. Only what runs reported is counted: usage that never arrived is not zero
 * here, it is missing. Buckets are UTC.
 */
export function SpaceUsage({ spaceId }: { spaceId: string }) {
  const [range, setRange] = useState<RangeKey>('7d')
  const [groupBy, setGroupBy] = useState<GroupBy>('role')
  // The range is fixed per mount and choice, so the query key doesn't change every render.
  // A page left open moves on: the range is recomputed every few minutes, and the query key changes when a bucket ends.
  const [tick, setTick] = useState(0)
  useEffect(() => { const t = setInterval(() => setTick((x) => x + 1), 5 * 60_000); return () => clearInterval(t) }, [])
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const r = useMemo(() => rangeOf(range), [range, tick])
  const usage = useSpaceUsage(spaceId, r.bucket, groupBy, r.from, r.to)
  const accounts = useSpaceAccounts(spaceId)
  const names = useMemo(() => Object.fromEntries((accounts.data ?? []).map((a) => [a.account_id, a.name ?? ''])), [accounts.data])
  const d = usage.data
  const chart = useMemo(() => (d ? buildChart(d.items, bucketStarts(d.from, d.to, d.bucket), groupBy === 'account' ? names : undefined) : null), [d, groupBy, names])
  const rows = useMemo(() => (d ? perGroup(d.items) : []), [d])
  const label = fmtBucket(r.bucket)
  const empty = !!d && d.totals.total_tokens === 0
  return (
    <Stack gap="md">
      <Group justify="space-between" gap="sm">
        <SegmentedControl aria-label="Time range" value={range} onChange={(v) => setRange(v as RangeKey)} data={RANGES.map((x) => ({ value: x.value, label: x.label }))} />
        <SegmentedControl aria-label="Group by" value={groupBy} onChange={(v) => setGroupBy(v as GroupBy)} data={GROUPS} />
      </Group>
      <ErrorAlert error={usage.error} />
      {d && (
        <SimpleGrid cols={{ base: 2, sm: 4 }} data-testid="usage-totals">
          <Tile label="Tokens" value={n(d.totals.total_tokens)} note="input, output and cache" primary />
          <Tile label="Input / output" value={`${compact(d.totals.input_tokens)} / ${compact(d.totals.output_tokens)}`} note="tokens" />
          <Tile label="Cache read / write" value={`${compact(d.totals.cache_read_tokens)} / ${compact(d.totals.cache_write_tokens)}`} note="tokens" />
          <Tile label="Estimated cost" value={estimate(d.totals.total_cost_usd)} note="list price, not a bill" />
        </SimpleGrid>
      )}
      {d?.next_cursor && <Alert color="owlYellow" variant="light" title="Not every row is shown">The range has more rows than the page loads. The totals cover the whole range; the chart and table do not. Choose a shorter range.</Alert>}
      <Card>
        <Title order={5} mb={4}>Tokens per {r.bucket}, by {groupBy}</Title>
        {empty ? <Text size="sm" c="dimmed" data-testid="usage-empty">No usage was reported in this range.</Text> : chart && (
          <>
            <UsageChart chart={chart} label={label} bucket={r.bucket} />
            <UsageLegend chart={chart} />
          </>
        )}
        <Text size="xs" c="dimmed" mt="xs">Buckets are UTC. Only usage that runs reported is counted.</Text>
      </Card>
      {d && !empty && (
        <Card p={0}>
          {/* The table can scroll sideways on a phone, and has nothing focusable in it: the region takes focus so a keyboard can scroll it. */}
          <Table.ScrollContainer minWidth={640} scrollAreaProps={{ viewportProps: { tabIndex: 0, role: 'region', 'aria-label': `Usage by ${groupBy}, table` } }}>
            <Table verticalSpacing="xs" aria-label={`Usage by ${groupBy}`}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{GROUPS.find((g) => g.value === groupBy)?.label}</Table.Th><Table.Th ta="right">Tokens</Table.Th><Table.Th ta="right">Input</Table.Th><Table.Th ta="right">Output</Table.Th>
                  <Table.Th ta="right">Cache read</Table.Th><Table.Th ta="right">Cache write</Table.Th><Table.Th ta="right">Requests</Table.Th><Table.Th ta="right">Estimated cost</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((g) => (
                  <Table.Tr key={g.group}>
                    <Table.Td>{groupLabel(g.group, groupBy === 'account' ? names : undefined)}</Table.Td>
                    <Table.Td ta="right" fw={600}>{n(g.total_tokens)}</Table.Td><Table.Td ta="right">{n(g.input_tokens)}</Table.Td><Table.Td ta="right">{n(g.output_tokens)}</Table.Td>
                    <Table.Td ta="right">{n(g.cache_read_tokens)}</Table.Td><Table.Td ta="right">{n(g.cache_write_tokens)}</Table.Td><Table.Td ta="right">{n(g.requests)}</Table.Td>
                    <Table.Td ta="right">{estimate(g.total_cost_usd)}</Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        </Card>
      )}
      {d && (
        <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />} title="Cost is an estimate">
          <Text size="sm">{d.cost_note}</Text>
        </Alert>
      )}
    </Stack>
  )
}

function Tile({ label, value, note, primary }: { label: string; value: string; note: string; primary?: boolean }) {
  return (
    <Card>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Text fw={primary ? 700 : 600} fz={primary ? 'xl' : 'lg'}>{value}</Text>
      <Text size="xs" c="dimmed">{note}</Text>
    </Card>
  )
}
