import { Alert, Badge, Button, Card, Group, SimpleGrid, Table, Text } from '@mantine/core'
import { IconDownload } from '@tabler/icons-react'
import { useQuery } from '@tanstack/react-query'
import { api, unwrap } from '@/api/client'
import { useMe } from '@/api/queries'
import { atLeast, roleIn } from '@/lib/access'

const n = (v?: number | null) => (v == null ? 'unknown' : v.toLocaleString('en-US'))
const bytes = (b: number) => (b >= 1 << 20 ? `${(b / (1 << 20)).toFixed(1)} MiB` : `${Math.max(1, Math.round(b / 1024))} KiB`)

/**
 * A run's usage (SM-REC-005): token counts the harness recorded, its own cost estimate with what it assumes, and the
 * stored native transcript. Unknown is shown as unknown, never as zero.
 */
export function RunUsageCard({ spaceId, runId }: { spaceId: string; runId: string }) {
  const canRead = atLeast(roleIn(useMe().data, spaceId), 'operator')
  const u = useQuery({ queryKey: ['usage', spaceId, runId], queryFn: async () =>
    unwrap(await api.GET('/spaces/{space_id}/runs/{run_id}/usage', { params: { path: { space_id: spaceId, run_id: runId } } })) })
  if (!u.data) return null
  const d = u.data
  return (
    <Card>
      <Group justify="space-between" mb={4}>
        <Text fw={600}>Usage</Text>
        <Badge variant="light" color={d.known ? 'owlGreen' : 'gray'}>{usageBadge(d.known, d.final, d.source)}</Badge>
      </Group>
      {!d.known ? (
        <Text size="sm" c="dimmed">Usage is unknown{d.note ? `: ${d.note}` : '.'} SpaceMan does not count unknown usage as zero.</Text>
      ) : (
        <Table>
          <Table.Thead><Table.Tr><Table.Th>Model</Table.Th><Table.Th>Requests</Table.Th><Table.Th>Input</Table.Th><Table.Th>Output</Table.Th><Table.Th>Cache write</Table.Th><Table.Th>Cache read</Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {d.models.map((m) => (
              <Table.Tr key={m.model}>
                <Table.Td className="mono">{m.model}</Table.Td><Table.Td>{m.requests}</Table.Td><Table.Td>{n(m.input_tokens)}</Table.Td>
                <Table.Td>{n(m.output_tokens)}</Table.Td><Table.Td>{n(m.cache_write_tokens)}</Table.Td><Table.Td>{n(m.cache_read_tokens)}</Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      )}
      {d.estimate && (
        <Text size="xs" c="dimmed" mt="xs">
          Estimated ${d.estimate.usd.toFixed(4)} by {d.estimate.source}. {d.estimate.assumptions}.{d.estimate.unknown_models ? ' Some usage had no known price.' : ''}
        </Text>
      )}
      {d.gateway && <GatewayUsage g={d.gateway} />}
      {d.transcript && (
        <Group justify="space-between" mt="xs">
          <Text size="xs" c="dimmed">
            Native transcript: {d.transcript.state}{d.transcript.state === 'stored' && `, ${bytes(d.transcript.size)}`}{d.transcript.truncated ? ' (start only)' : ''}. It may contain secrets.
          </Text>
          {d.transcript.state === 'stored' && canRead && (
            <Button component="a" size="xs" variant="default" leftSection={<IconDownload size={14} />} href={`/api/v1/spaces/${spaceId}/runs/${runId}/transcript`}>Transcript</Button>
          )}
        </Group>
      )}
    </Card>
  )
}

/** The run usage card's badge: where the figures come from, and whether more may come. */
export function usageBadge(known: boolean, final: boolean, source: string): string {
  if (!known) return 'unknown'
  const from = source === 'harbor-pilot' ? 'from Harbor Pilot' : 'from the transcript'
  return final ? from : `${from}, so far`
}

/**
 * What the Harbor Pilot gateway observed for a gateway run (HP-T5), read back after the run ended. It fills the run's
 * usage only when the harness reported none; next to a harness's own figures it is shown for comparison.
 */
export function GatewayUsage({ g }: { g: { state: string; requests: number; unknown_usage_requests: number; costs: { kind: string; currency: string; amount: string }[]; models: { input_tokens?: number | null; output_tokens?: number | null }[]; read_at?: string; note?: string } }) {
  const input = g.models.reduce((a, m) => a + (m.input_tokens ?? 0), 0)
  const output = g.models.reduce((a, m) => a + (m.output_tokens ?? 0), 0)
  const state = g.state === 'final' ? 'final' : g.state === 'pending' ? (g.read_at ? 'not final yet' : 'not read yet') : g.state
  return (
    <Text size="xs" c="dimmed" mt="xs" data-testid="gateway-usage">
      Harbor Pilot (gateway-observed, {state}):{' '}
      {g.read_at ? <>{g.requests} request(s), {n(input)} input and {n(output)} output tokens{g.costs.length > 0 && <>, {g.costs.map((c) => `${c.amount} ${c.currency} (${c.kind.replace(/_/g, ' ')})`).join(', ')}</>}
        {g.unknown_usage_requests > 0 && <>; {g.unknown_usage_requests} without reported usage</>}.</> : 'read after the run ends.'}
      {g.note && <> {g.note}.</>}
    </Text>
  )
}

/** A space's usage over its recent runs: totals of known usage, and how many runs' usage is unknown. */
export function SpaceUsageCard({ spaceId }: { spaceId: string }) {
  const s = useQuery({ queryKey: ['usage', spaceId], queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/usage', { params: { path: { space_id: spaceId } } })) })
  if (!s.data) return null
  const d = s.data
  return (
    <Card mb="md">
      <Text size="xs" c="dimmed" tt="uppercase" fw={600} mb="xs">Usage</Text>
      <SimpleGrid cols={{ base: 2, sm: 4 }}>
        <Stat label="Output tokens" value={n(d.output_tokens)} hint={`input ${n(d.input_tokens)}`} />
        <Stat label="Cache" value={n(d.cache_read_tokens)} hint={`read · ${n(d.cache_write_tokens)} written`} />
        <Stat label="Runs with known usage" value={String(d.known_runs)} hint={`${d.unknown_runs} unknown`} />
        <Stat label="Harness estimates" value={d.estimated_runs > 0 ? `$${d.estimated_usd.toFixed(2)}` : 'none'} hint={d.estimated_runs > 0 ? `from ${d.estimated_runs} run(s), at list prices` : undefined} />
      </SimpleGrid>
      {d.unknown_runs > 0 && (
        <Alert color="owlYellow" variant="light" mt="xs" p="xs">
          <Text size="xs">Totals cover runs with known usage only. {d.unknown_runs} run(s) have unknown usage (not zero): their harness kept no records SpaceMan can read.</Text>
        </Alert>
      )}
    </Card>
  )
}

function Stat({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div>
      <Text size="xs" c="dimmed">{label}</Text>
      <Text fw={600}>{value}</Text>
      {hint && <Text size="xs" c="dimmed">{hint}</Text>}
    </div>
  )
}
