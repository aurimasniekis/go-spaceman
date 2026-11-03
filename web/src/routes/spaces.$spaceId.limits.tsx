import { Alert, Badge, Button, Card, Group, Modal, NumberInput, SegmentedControl, SimpleGrid, Stack, Table, Text, Title, Tooltip } from '@mantine/core'
import { IconInfoCircle, IconPencil } from '@tabler/icons-react'
import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { api, ifMatch, unwrap, type Limit, type Limits } from '@/api/client'
import { keys, useLimits, useMe, useSpace } from '@/api/queries'
import { AnchorLink } from '@/components/AnchorLink'
import { errorMessage, ErrorAlert } from '@/components/ErrorAlert'
import { PageHeader } from '@/components/PageHeader'
import { limitValue, usd } from '@/features/limits/format'
import { SpaceNav } from '@/features/spaces/SpaceNav'
import { atLeast, roleIn } from '@/lib/access'
import { ago } from '@/lib/format'
import { notifyOk } from '@/lib/notify'

export const Route = createFileRoute('/spaces/$spaceId/limits')({ component: LimitsPage })

const SOURCE: Record<string, { label: string; color: string }> = {
  installation: { label: 'Installation', color: 'gray' },
  space: { label: 'This space', color: 'owlBlue' },
  queue_policy: { label: 'Queue policy', color: 'owlPurple' },
}

const BUDGET_STATE: Record<string, { label: string; color: string }> = {
  none: { label: 'No budget', color: 'gray' },
  ok: { label: 'Within budget', color: 'owlGreen' },
  warning: { label: 'Warning', color: 'owlYellow' },
  exceeded: { label: 'Over budget: new runs refused', color: 'owlRed' },
}

function LimitsPage() {
  const { spaceId } = Route.useParams()
  const space = useSpace(spaceId)
  const limits = useLimits(spaceId)
  const me = useMe().data
  const canWrite = atLeast(roleIn(me, spaceId), 'operator')
  const [editing, setEditing] = useState(false)
  const l = limits.data

  return (
    <>
      <PageHeader title={space.data?.name ?? 'Space'} description="The limits in force in this space: installation defaults, this space's overrides, and where each is enforced."
        actions={canWrite && l && <Button leftSection={<IconPencil size={14} />} onClick={() => setEditing(true)}>Edit limits</Button>} />
      <SpaceNav spaceId={spaceId} />
      <ErrorAlert error={limits.error} />
      {l && (
        <Stack gap="md">
          <BudgetCard l={l} />
          <Card p={0}>
            <Table verticalSpacing="xs">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Limit</Table.Th>
                  <Table.Th>Value</Table.Th>
                  <Table.Th>Source</Table.Th>
                  <Table.Th>Default</Table.Th>
                  <Table.Th>Enforced at</Table.Th>
                  <Table.Th>When lowered</Table.Th>
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {l.limits.map((x) => (
                  <Table.Tr key={x.key}>
                    <Table.Td maw={260}>
                      <Text size="sm" fw={500}>{x.label}</Text>
                      <Text size="xs" c="dimmed">{x.scope}</Text>
                    </Table.Td>
                    <Table.Td><Text size="sm" fw={600} style={{ whiteSpace: 'nowrap' }}>{limitValue(x.value, x.unit, x.zero_means_none)}</Text></Table.Td>
                    <Table.Td>
                      <Badge variant="light" color={SOURCE[x.source]?.color}>{SOURCE[x.source]?.label ?? x.source}</Badge>
                      {x.edit_at === 'queue_policy' && (
                        <Text size="xs" mt={2}><AnchorLink to="/spaces/$spaceId/queue" params={{ spaceId }} size="xs">Edit in the queue policy</AnchorLink></Text>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Tooltip label={`Configuration: ${x.config_key}`}><Text size="xs" c="dimmed" style={{ whiteSpace: 'nowrap' }}>{limitValue(x.default, x.unit, x.zero_means_none)}</Text></Tooltip>
                    </Table.Td>
                    <Table.Td maw={180}><Text size="xs">{x.enforced}</Text></Table.Td>
                    <Table.Td maw={300}><Text size="xs" c="dimmed">{x.on_lower}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Card>
          <Text size="sm">
            When the concurrent-runs limit is lowered: <b>{l.on_lower === 'stop' ? 'stop the newest runs beyond it' : 'let running runs finish (drain)'}</b>.
            {' '}No other limit stops running work when it is lowered.
            {l.updated_at && <Text span size="xs" c="dimmed"> Overrides changed {ago(l.updated_at)} by {l.updated_by}.</Text>}
          </Text>
          <Card>
            <Title order={5} mb="xs">Set elsewhere</Title>
            <Table>
              <Table.Thead>
                <Table.Tr><Table.Th>Limit</Table.Th><Table.Th>Value</Table.Th><Table.Th>Configured in</Table.Th></Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {l.related.map((r) => (
                  <Table.Tr key={r.key}>
                    <Table.Td><Text size="sm">{r.label}</Text></Table.Td>
                    <Table.Td><Text size="sm">{r.value}</Text></Table.Td>
                    <Table.Td><Text size="xs" c="dimmed">{r.edit_at}</Text></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Card>
        </Stack>
      )}
      {editing && l && <EditLimits spaceId={spaceId} l={l} onClose={() => setEditing(false)} />}
    </>
  )
}

function BudgetCard({ l }: { l: Limits }) {
  const b = l.budget
  const st = BUDGET_STATE[b.state] ?? BUDGET_STATE.none
  return (
    <Card>
      <Group justify="space-between" mb="xs">
        <Title order={5}>Budget, last {b.window_hours} hours</Title>
        <Badge color={st.color}>{st.label}</Badge>
      </Group>
      <SimpleGrid cols={{ base: 1, sm: 3 }} mb="sm">
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Reported tokens</Text>
          <Text fw={600}>{b.tokens.toLocaleString('en-US')}{b.tokens_limit > 0 ? ` of ${b.tokens_limit.toLocaleString('en-US')}` : ''}</Text>
          {b.tokens_limit === 0 && <Text size="xs" c="dimmed">no token budget</Text>}
        </div>
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Estimated cost</Text>
          <Text fw={600}>{usd(b.cost_cents)}{b.cost_limit_cents > 0 ? ` of ${usd(b.cost_limit_cents)}` : ''}</Text>
          <Text size="xs" c="dimmed">{b.estimated_runs} run(s) with a harness estimate{b.cost_limit_cents === 0 ? ' · no cost budget' : ''}</Text>
        </div>
        <div>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Runs started</Text>
          <Text fw={600}>{b.runs}</Text>
          <Text size="xs" c={b.unknown_runs > 0 ? 'owlYellow' : 'dimmed'}>{b.unknown_runs} with unknown usage (not counted)</Text>
        </div>
      </SimpleGrid>
      <Alert color="owlBlue" variant="light" icon={<IconInfoCircle size={16} />} title="Not a hard currency limit">
        <Text size="sm">{b.note}</Text>
        <Text size="xs" mt={4}>Operators are warned at {b.warn_percent} % of a budget, in this space's Messages.</Text>
      </Alert>
    </Card>
  )
}

function EditLimits({ spaceId, l, onClose }: { spaceId: string; l: Limits; onClose: () => void }) {
  const qc = useQueryClient()
  const editable = l.limits.filter((x) => x.overridable)
  const [values, setValues] = useState<Record<string, number | ''>>(() =>
    Object.fromEntries(editable.map((x) => [x.key, x.override ?? ''])))
  const [onLower, setOnLower] = useState<'drain' | 'stop'>(l.on_lower)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const save = async () => {
    setError('')
    setSaving(true)
    try {
      const body = { values: Object.fromEntries(Object.entries(values).filter(([, v]) => v !== '')) as Record<string, number>, on_lower: onLower }
      const header = l.version > 0 ? { 'If-Match': ifMatch(l.version) } : {}
      const next = unwrap(await api.PUT('/spaces/{space_id}/limits', { params: { path: { space_id: spaceId }, header }, body }))
      qc.setQueryData(keys.limits(spaceId), next)
      notifyOk('Limits saved')
      onClose()
    } catch (e) {
      setError(errorMessage(e))
    } finally {
      setSaving(false)
    }
  }
  const input = (x: Limit) => (
    <NumberInput key={x.key} label={x.label} description={`${x.scope}${x.zero_means_none ? ' (0: no limit)' : ''}`} min={x.min} max={x.max} allowDecimal={false}
      thousandSeparator="," placeholder={`Default: ${limitValue(x.default, x.unit, x.zero_means_none)}`} value={values[x.key]}
      onChange={(v) => setValues((s) => ({ ...s, [x.key]: typeof v === 'number' ? v : '' }))} />
  )
  return (
    <Modal opened onClose={onClose} title="Edit this space's limits" size="xl">
      <Stack gap="sm">
        <Text size="sm" c="dimmed">Leave a field empty to use the installation default. Lowering a limit refuses new work beyond it; running work continues.</Text>
        <SimpleGrid cols={{ base: 1, sm: 2 }}>{editable.map(input)}</SimpleGrid>
        <div>
          <Text size="sm" fw={500}>When the concurrent-runs limit is lowered</Text>
          <SegmentedControl mt={4} value={onLower} onChange={(v) => setOnLower(v as 'drain' | 'stop')}
            data={[{ value: 'drain', label: 'Drain: let runs finish' }, { value: 'stop', label: 'Stop the newest runs' }]} />
          <Text size="xs" c="dimmed" mt={4}>Stop interrupts the newest worker and diagnostic runs beyond the new limit, then kills them after the grace. The oldest keep running.</Text>
        </div>
        <Text size="xs" c="dimmed">Budgets use usage reported by the harnesses, which arrives late or not at all: they warn and stop new runs, never running ones. Changes are audited.</Text>
        {error && <Text c="owlRed" size="sm" role="alert">{error}</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={saving} onClick={() => void save()}>Save</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
