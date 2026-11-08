import { Alert, Badge, Card, Group, Progress, SimpleGrid, Stack, Table, Text, Title, Tooltip } from '@mantine/core'
import { IconAlertTriangle, IconInfoCircle } from '@tabler/icons-react'
import type { HarborPilotStatus } from '@/api/client'
import { useHarborPilotStatus } from '@/api/queries'
import { ago } from '@/lib/format'
import { budgetBadge, costLabel, harnessLabel, scopeLabel, STATUS_BADGE, usedPercent, windowLabel } from './format'

/** The Limits page's Harbor Pilot card (HP-T5): nothing when the space doesn't route runs through Harbor Pilot. */
export function HarborPilotLimitsCard({ spaceId }: { spaceId: string }) {
  const st = useHarborPilotStatus(spaceId).data
  if (!st || st.state === 'off') return null
  return <HarborPilotLimits st={st} />
}

const n = (v: number) => v.toLocaleString('en-US')

/**
 * Harbor Pilot's budgets covering a space, labelled with their source. Budgets enforced with block are hard limits:
 * Harbor Pilot refuses requests over them, and SpaceMan refuses launches once one is used up. The project's usage
 * counts every Harbor Pilot client of the project, not only this space's runs.
 */
export function HarborPilotLimits({ st }: { st: HarborPilotStatus }) {
  const badge = STATUS_BADGE[st.state] ?? STATUS_BADGE.unavailable
  const blocking = st.budgets.find((b) => b.blocking)
  return (
    <Card data-testid="harbor-pilot-limits">
      <Group justify="space-between" mb="xs" wrap="wrap">
        <div>
          <Title order={5}>Harbor Pilot budgets</Title>
          <Text size="xs" c="dimmed">
            Hard limits enforced by Harbor Pilot · project <span className="mono">{st.project_id}</span> · {st.harnesses.map(harnessLabel).join(', ')}
          </Text>
        </div>
        <Group gap="xs">
          <Badge variant="light" color="owlPurple">Source: Harbor Pilot</Badge>
          <Badge color={badge.color} data-testid="harbor-pilot-state">{badge.label}</Badge>
        </Group>
      </Group>
      {st.state === 'unavailable' && (
        <Alert color="gray" variant="light" icon={<IconAlertTriangle size={16} />} mb="sm" title="Budget status can't be read">
          <Text size="sm">{st.unavailable}</Text>
          <Text size="xs" mt={4}>
            {st.on_budget_status_unavailable === 'refuse'
              ? 'This space refuses launches while budget status is unavailable.'
              : 'Launches proceed; Harbor Pilot still enforces its budgets when it mints a run token and on every request.'}
          </Text>
        </Alert>
      )}
      {blocking && (
        <Alert color="owlRed" variant="light" icon={<IconAlertTriangle size={16} />} mb="sm" title="New runs are refused">
          <Text size="sm">
            Budget “{blocking.name}” is used up ({blocking.spent} of {blocking.limit} {blocking.currency}). It resets {ago(blocking.period_end)} ({new Date(blocking.period_end).toLocaleString()}).
          </Text>
        </Alert>
      )}
      {st.state !== 'unavailable' && (st.budgets.length === 0 ? (
        <Text size="sm" c="dimmed" mb="sm">No Harbor Pilot budget covers this space&apos;s project, workspace or client binding.</Text>
      ) : (
        <Table.ScrollContainer minWidth={560} mb="sm">
          <Table verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr><Table.Th>Budget</Table.Th><Table.Th>Enforcement</Table.Th><Table.Th>Spent</Table.Th><Table.Th>State</Table.Th><Table.Th>Resets</Table.Th></Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {st.budgets.map((b) => {
                const bb = budgetBadge(b.state, b.enforcement)
                return (
                  <Table.Tr key={b.budget_id} data-testid="harbor-pilot-budget">
                    <Table.Td maw={240}>
                      <Text size="sm" fw={500}>{b.name}</Text>
                      <Text size="xs" c="dimmed">{scopeLabel(b.scope, b.scope_id)} · per {b.period}</Text>
                    </Table.Td>
                    <Table.Td>
                      <Badge variant="light" color={b.enforcement === 'block' ? 'owlRed' : 'gray'}>{b.enforcement === 'block' ? 'Hard limit' : 'Warn only'}</Badge>
                    </Table.Td>
                    <Table.Td miw={160}>
                      <Text size="sm" style={{ whiteSpace: 'nowrap' }}>{b.spent} of {b.limit} {b.currency}</Text>
                      <Tooltip label={`${b.reserved} ${b.currency} reserved by requests in flight`}>
                        <Progress value={usedPercent(b.fraction)} color={bb.color} size="sm" mt={4} aria-label={`${usedPercent(b.fraction)} % used`} />
                      </Tooltip>
                    </Table.Td>
                    <Table.Td><Badge color={bb.color} variant="light">{bb.label}</Badge></Table.Td>
                    <Table.Td><Text size="xs" style={{ whiteSpace: 'nowrap' }}>{new Date(b.period_end).toLocaleString()}</Text></Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
      ))}
      {st.usage && (
        <Stack gap={4} mb="sm" data-testid="harbor-pilot-usage">
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Project usage, {windowLabel(st.usage.window)} · all clients</Text>
          <SimpleGrid cols={{ base: 2, sm: 4 }}>
            <Stat label="Requests" value={n(st.usage.requests)} hint={st.usage.unknown_usage_requests > 0 ? `${n(st.usage.unknown_usage_requests)} without reported usage` : undefined} />
            <Stat label="Input tokens" value={n(st.usage.input_tokens)} hint={`cache read ${n(st.usage.cache_read_tokens)}`} />
            <Stat label="Output tokens" value={n(st.usage.output_tokens)} />
            <Stat label="Cost" value={st.usage.costs.length > 0 ? costLabel(st.usage.costs[0]) : 'none'} hint={st.usage.costs.slice(1).map(costLabel).join(' · ') || undefined} />
          </SimpleGrid>
          <Text size="xs" c="dimmed">Every Harbor Pilot client of the project counts here, not only this space&apos;s runs.</Text>
        </Stack>
      )}
      {st.usage_unavailable && <Text size="xs" c="dimmed" mb="sm">Project usage can&apos;t be read: {st.usage_unavailable}</Text>}
      <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />} p="xs">
        <Text size="xs">
          Budgets marked hard limit are enforced by Harbor Pilot: it refuses requests over them, and SpaceMan refuses to launch this space&apos;s
          {' '}{st.harnesses.map(harnessLabel).join(' and ')} runs once one is used up. Warn-only budgets never refuse.
          {st.fetched_at && <> Read from Harbor Pilot {ago(st.fetched_at)}.</>}
        </Text>
      </Alert>
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
