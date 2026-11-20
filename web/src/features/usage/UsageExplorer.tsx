import { Alert, Badge, Card, CloseButton, Group, SegmentedControl, SimpleGrid, Stack, Table, Text, Title, Anchor } from '@mantine/core'
import { IconInfoCircle } from '@tabler/icons-react'
import { useEffect, useMemo, useState } from 'react'
import { useAllAccountWindows, useSpaces, useUsageSeries } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { ErrorAlert } from '@/components/ErrorAlert'
import { AccountWindowsTable } from './AccountWindowsTable'
import { RANGES, rangeOf } from './range'
import { bucketStarts, buildChart, compact, estimate, groupLabel } from './series'
import { FILTER_KEYS, GROUPS, filterOf, harnessName, notCountedLines, perGroup, resolveUsage, serializeUsage, toggleFilter, type FilterKey, type GroupBy, type UsageSearch } from './state'
import { UsageChart, UsageLegend } from './UsageChart'

const n = (v: number) => v.toLocaleString('en-US')
const SPACE_GROUPS: GroupBy[] = ['role', 'account', 'model', 'harness']
const FILTER_NOUN: Record<FilterKey, string> = { space: 'Space', account: 'Account', model: 'Model', harness: 'Harness' }

const fmtBucket = (bucket: 'hour' | 'day') => (iso: string) => {
  const d = new Date(iso)
  return bucket === 'hour' ? `${String(d.getUTCHours()).padStart(2, '0')}:00` : d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' })
}

/**
 * Token usage from the usage ledger, for one space (`spaceId`) or for every space the caller can read. Tokens are the
 * primary unit; the cost is the harness's own list-price estimate and is labelled so. Only what runs reported is
 * counted: usage that never arrived is missing, not zero, and runs whose harness books nothing are listed as "not
 * counted". The state (range, grouping, filters) lives in `search`, which the page keeps in the URL. Buckets are UTC.
 */
export function UsageExplorer({ spaceId, search, onSearch, names }: {
  spaceId?: string
  search: UsageSearch
  onSearch: (next: UsageSearch) => void
  /** Display names known to the caller (a space's accounts); the response's own labels win. */
  names?: Record<string, string>
}) {
  const everywhere = !spaceId
  const defaultGroup: GroupBy = everywhere ? 'space' : 'role'
  const st = useMemo(() => resolveUsage(search, defaultGroup, everywhere ? undefined : SPACE_GROUPS), [search, defaultGroup, everywhere])
  const set = (next: typeof st) => onSearch(serializeUsage(next, defaultGroup))
  // A page left open moves on: the range is recomputed every few minutes, and the query key changes when a bucket ends.
  const [tick, setTick] = useState(0)
  useEffect(() => { const t = setInterval(() => setTick((x) => x + 1), 5 * 60_000); return () => clearInterval(t) }, [])
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const r = useMemo(() => rangeOf(st.range), [st.range, tick])
  const usage = useUsageSeries(spaceId, {
    bucket: r.bucket, group_by: st.group, from: r.from, to: r.to,
    ...(everywhere ? { space_id: st.space.length ? st.space : undefined, account_id: st.account.length ? st.account : undefined, model: st.model.length ? st.model : undefined, harness: st.harness.length ? st.harness : undefined } : {}),
  })
  const windows = useAllAccountWindows(everywhere)
  const d = usage.data
  const known = useMemo(() => ({ ...names, ...Object.fromEntries((windows.data ?? []).map((a) => [a.account_id, a.name])), ...d?.labels }), [names, windows.data, d?.labels])
  const chart = useMemo(() => (d ? buildChart(d.items, bucketStarts(d.from, d.to, d.bucket), known) : null), [d, known])
  const rows = useMemo(() => (d ? perGroup(d.items) : []), [d])
  const tokens = useMemo(() => (d && st.group === 'account' ? Object.fromEntries(rows.map((g) => [g.group, g.total_tokens])) : undefined), [d, st.group, rows])
  const lines = notCountedLines(d?.not_counted)
  const label = fmtBucket(r.bucket)
  const empty = !!d && d.totals.total_tokens === 0
  const groups = everywhere ? GROUPS : GROUPS.filter((g) => SPACE_GROUPS.includes(g.value))
  const groupName = GROUPS.find((g) => g.value === st.group)?.label ?? st.group
  const filterKey = filterOf(st.group)
  return (
    <Stack gap="md">
      <Group justify="space-between" gap="sm">
        <SegmentedControl aria-label="Time range" value={st.range} onChange={(v) => set({ ...st, range: v as typeof st.range })} data={RANGES.map((x) => ({ value: x.value, label: x.label }))} />
        <SegmentedControl aria-label="Group by" value={st.group} onChange={(v) => set({ ...st, group: v as GroupBy })} data={groups} />
      </Group>
      {everywhere && <FilterChips st={st} known={known} onRemove={(k, v) => set(toggleFilter(st, k, v))} />}
      <ErrorAlert error={usage.error} />
      {d && (
        <SimpleGrid cols={{ base: 2, sm: 4 }} data-testid="usage-totals">
          <Tile label="Tokens" value={n(d.totals.total_tokens)} note="input, output and cache" primary />
          <Tile label="Input / output" value={`${compact(d.totals.input_tokens)} / ${compact(d.totals.output_tokens)}`} note="tokens" />
          <Tile label="Cache read / write" value={`${compact(d.totals.cache_read_tokens)} / ${compact(d.totals.cache_write_tokens)}`} note="tokens" />
          <Tile label="Estimated cost" value={estimate(d.totals.total_cost_usd)} note="list price, not a bill" />
        </SimpleGrid>
      )}
      {d?.next_cursor && <Alert color="owlYellow" variant="light" title="Not every row is shown">The range has more rows than the page loads. The totals cover the whole range; the chart and table do not. Choose a shorter range or add a filter.</Alert>}
      {lines.length > 0 && (
        <Alert color="owlYellow" variant="light" icon={<IconInfoCircle size={16} />} title="Not counted" data-testid="usage-not-counted">
          <Text size="sm">Usage of these runs is not in the figures above, so the totals are lower than what was used. It is missing, not zero.</Text>
          <ul style={{ margin: '4px 0 0', paddingLeft: 20 }}>{lines.map((l) => <li key={l}><Text size="sm" span>{l}</Text></li>)}</ul>
          <Text size="xs" c="dimmed" mt={4}>{d?.not_counted.some((i) => i.harbor_pilot_runs > 0) ? 'Runs read back from Harbor Pilot have their figures on the run page only. ' : ''}Only Claude Code usage from a run's transcript reaches this page.</Text>
        </Alert>
      )}
      <Card>
        <Title order={5} mb={4}>Tokens per {r.bucket}, by {groupName.toLowerCase()}</Title>
        {empty ? <Text size="sm" c="dimmed" data-testid="usage-empty">No usage was reported in this range{FILTER_KEYS.some((k) => st[k].length) ? ' for these filters' : ''}.</Text> : chart && (
          <>
            <UsageChart chart={chart} label={label} bucket={r.bucket} />
            <UsageLegend chart={chart} />
          </>
        )}
        <Text size="xs" c="dimmed" mt="xs">Buckets are UTC. Only usage that runs reported is counted.</Text>
      </Card>
      {d && !empty && (
        <Card p={0}>
          {/* The table can scroll sideways on a phone; the region takes focus so a keyboard can scroll it. */}
          <Table.ScrollContainer minWidth={640} scrollAreaProps={{ viewportProps: { tabIndex: 0, role: 'region', 'aria-label': `Usage by ${st.group}, table` } }}>
            <Table verticalSpacing="xs" aria-label={`Usage by ${st.group}`}>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>{groupName}</Table.Th><Table.Th ta="right">Tokens</Table.Th><Table.Th ta="right">Input</Table.Th><Table.Th ta="right">Output</Table.Th>
                  <Table.Th ta="right">Cache read</Table.Th><Table.Th ta="right">Cache write</Table.Th><Table.Th ta="right">Requests</Table.Th><Table.Th ta="right">Estimated cost</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((g) => {
                  const text = st.group === 'harness' ? harnessName(g.group) : groupLabel(g.group, known)
                  return (
                    <Table.Tr key={g.group}>
                      <Table.Td>
                        {everywhere && st.group === 'space' && g.group ? <AnchorLink to="/spaces/$spaceId/usage" params={{ spaceId: g.group }}>{text}</AnchorLink>
                          : everywhere && filterKey && g.group ? (
                            <Anchor component="button" type="button" size="sm" onClick={() => set(toggleFilter(st, filterKey, g.group))} aria-label={`Filter by ${text}`}>{text}</Anchor>
                          ) : text}
                      </Table.Td>
                      <Table.Td ta="right" fw={600}>{n(g.total_tokens)}</Table.Td><Table.Td ta="right">{n(g.input_tokens)}</Table.Td><Table.Td ta="right">{n(g.output_tokens)}</Table.Td>
                      <Table.Td ta="right">{n(g.cache_read_tokens)}</Table.Td><Table.Td ta="right">{n(g.cache_write_tokens)}</Table.Td><Table.Td ta="right">{n(g.requests)}</Table.Td>
                      <Table.Td ta="right">{estimate(g.total_cost_usd)}</Table.Td>
                    </Table.Tr>
                  )
                })}
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
      {everywhere && (
        <>
          <ErrorAlert error={windows.error} />
          {windows.data && <AccountWindowsTable items={windows.data} tokens={tokens} />}
        </>
      )}
    </Stack>
  )
}

/** The active filters as removable chips, named from the response's labels or the spaces the caller reads. */
function FilterChips({ st, known, onRemove }: { st: ReturnType<typeof resolveUsage>; known: Record<string, string>; onRemove: (k: FilterKey, v: string) => void }) {
  const spaces = useSpaces()
  const all = { ...Object.fromEntries((spaces.data ?? []).map((s) => [s.id, s.name])), ...known }
  const active = FILTER_KEYS.flatMap((k) => st[k].map((v) => ({ k, v })))
  if (active.length === 0) return <Text size="xs" c="dimmed">Select an account, model or harness in the table to filter by it.</Text>
  return (
    <Group gap="xs" role="list" aria-label="Filters" data-testid="usage-filters">
      {active.map(({ k, v }) => {
        const text = k === 'harness' ? harnessName(v) : (all[v] ?? v)
        return (
          <Badge key={`${k}:${v}`} role="listitem" variant="light" size="lg" tt="none" rightSection={<CloseButton size="xs" aria-label={`Remove filter ${FILTER_NOUN[k]} ${text}`} onClick={() => onRemove(k, v)} />}>
            {FILTER_NOUN[k]}: {text}
          </Badge>
        )
      })}
    </Group>
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
