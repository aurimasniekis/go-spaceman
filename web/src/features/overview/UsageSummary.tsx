import { Card, Group, SimpleGrid, Stack, Text } from '@mantine/core'
import type { GlobalUsageSeries } from '@/api/client'
import { AnchorLink } from '@/components/AnchorLink'
import { compact, groupLabel } from '@/features/usage/series'
import { delta, summarize } from './summary'

/** A 14-day sparkline of daily tokens; a bar's exact figure is in its title and in the lists beside it. */
function Sparkline({ days }: { days: number[] }) {
  const max = Math.max(1, ...days)
  const W = 280
  const H = 48
  const slot = W / days.length
  return (
    <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} preserveAspectRatio="none" role="img" aria-label={`Tokens per day, last 14 days; most ${compact(Math.max(...days))}`} style={{ display: 'block', maxWidth: '100%' }}>
      {days.map((v, i) => {
        const h = v > 0 ? Math.max(2, (v / max) * (H - 4)) : 1
        return <rect key={i} x={i * slot + 2} y={H - h} width={slot - 4} height={h} rx={2} fill="light-dark(#2a78d6, #3987e5)" opacity={v > 0 ? 1 : 0.3}><title>{v.toLocaleString('en-US')} tokens</title></rect>
      })}
    </svg>
  )
}

function TopList({ title, rows }: { title: string; rows: { group: string; tokens: number }[] }) {
  return (
    <Stack gap={4} style={{ minWidth: 0 }}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{title}</Text>
      {rows.length === 0 ? <Text size="sm" c="dimmed">No usage</Text> : (
        <ul style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          {rows.map((r) => (
            <li key={r.group} style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
              <Text size="sm" truncate style={{ minWidth: 0 }}>{r.group}</Text>
              <Text size="sm" fw={600} style={{ flexShrink: 0 }} title={`${r.tokens.toLocaleString('en-US')} tokens`}>{compact(r.tokens)}</Text>
            </li>
          ))}
        </ul>
      )}
    </Stack>
  )
}

/**
 * Tokens today and over the last 7 days (with the change against the 7 days before), a 14-day sparkline and the top 5
 * spaces and models (plan §2.2, Q7: tokens only, never a cost). `bySpace` and `byModel` are the two daily reads.
 */
export function UsageSummary({ bySpace, byModel, from }: { bySpace?: GlobalUsageSeries; byModel?: GlobalUsageSeries; from: string }) {
  const s = summarize(bySpace?.items ?? [], from)
  const m = summarize(byModel?.items ?? [], from)
  const d = delta(s.week, s.previous)
  const names = bySpace?.labels
  return (
    <Card data-testid="usage-summary">
      <Group justify="space-between" mb={6} wrap="nowrap">
        <Text fw={600}>Usage</Text>
        <AnchorLink to={'/usage' as never} size="sm">Open Usage</AnchorLink>
      </Group>
      <SimpleGrid cols={{ base: 1, sm: 2 }} mb="sm">
        <Group gap="xl" wrap="nowrap" align="flex-start">
          <div>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Today</Text>
            <Text fw={600} size="xl" title={`${s.today.toLocaleString('en-US')} tokens`}>{compact(s.today)}</Text>
          </div>
          <div>
            <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Last 7 days</Text>
            <Text fw={600} size="xl" title={`${s.week.toLocaleString('en-US')} tokens`}>{compact(s.week)}</Text>
            <Text size="xs" c="dimmed" data-testid="usage-delta">
              {d ? `${d.up ? '▲' : d.pct === 0 ? '' : '▼'} ${d.pct} % vs the 7 days before` : 'no earlier period to compare'}
            </Text>
          </div>
        </Group>
        <Sparkline days={s.days} />
      </SimpleGrid>
      <SimpleGrid cols={{ base: 1, sm: 2 }}>
        <TopList title="Top spaces (7 d)" rows={s.top.map((r) => ({ ...r, group: groupLabel(r.group, names) }))} />
        <TopList title="Top models (7 d)" rows={m.top.map((r) => ({ ...r, group: groupLabel(r.group) }))} />
      </SimpleGrid>
      <Text size="xs" c="dimmed" mt="xs">Tokens only. Runs whose harness books no usage are not counted.</Text>
    </Card>
  )
}
